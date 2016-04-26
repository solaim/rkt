// Copyright 2014 The rkt Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//+build linux

package common

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/coreos/rkt/pkg/acl"
	"github.com/coreos/rkt/pkg/group"
	"github.com/coreos/rkt/pkg/passwd"
	stage1commontypes "github.com/coreos/rkt/stage1/common/types"

	"github.com/appc/spec/schema"
	"github.com/appc/spec/schema/types"
	"github.com/coreos/go-systemd/unit"
	"github.com/hashicorp/errwrap"

	"github.com/coreos/rkt/common"
	"github.com/coreos/rkt/common/cgroup"
	"github.com/coreos/rkt/pkg/uid"
)

const (
	// FlavorFile names the file storing the pod's flavor
	FlavorFile    = "flavor"
	SharedVolPerm = os.FileMode(0755)
)

var (
	defaultEnv = map[string]string{
		"PATH":    "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"SHELL":   "/bin/sh",
		"USER":    "root",
		"LOGNAME": "root",
		"HOME":    "/root",
	}

	// appDefaultCapabilities defines a restricted set of capabilities given to
	// apps by default.
	appDefaultCapabilities, _ = types.NewLinuxCapabilitiesRetainSet([]string{
		"CAP_AUDIT_WRITE",
		"CAP_CHOWN",
		"CAP_DAC_OVERRIDE",
		"CAP_FSETID",
		"CAP_FOWNER",
		"CAP_KILL",
		"CAP_MKNOD",
		"CAP_NET_RAW",
		"CAP_NET_BIND_SERVICE",
		"CAP_SETUID",
		"CAP_SETGID",
		"CAP_SETPCAP",
		"CAP_SETFCAP",
		"CAP_SYS_CHROOT",
	}...)
)

// execEscape uses Golang's string quoting for ", \, \n, and regex for special cases
func execEscape(i int, str string) string {
	escapeMap := map[string]string{
		`'`: `\`,
	}

	if i > 0 { // These are escaped only after the first argument
		escapeMap[`$`] = `$`
	}

	escArg := fmt.Sprintf("%q", str)
	for k := range escapeMap {
		reStr := `([` + regexp.QuoteMeta(k) + `])`
		re := regexp.MustCompile(reStr)
		escArg = re.ReplaceAllStringFunc(escArg, func(s string) string {
			escaped := escapeMap[s] + s
			return escaped
		})
	}
	return escArg
}

// quoteExec returns an array of quoted strings appropriate for systemd execStart usage
func quoteExec(exec []string) string {
	if len(exec) == 0 {
		// existing callers always include at least the binary so this shouldn't occur.
		panic("empty exec")
	}

	var qexec []string
	for i, arg := range exec {
		escArg := execEscape(i, arg)
		qexec = append(qexec, escArg)
	}
	return strings.Join(qexec, " ")
}

// WriteDefaultTarget writes the default.target unit file
// which is responsible for bringing up the applications
func WriteDefaultTarget(p *stage1commontypes.Pod) error {
	opts := []*unit.UnitOption{
		unit.NewUnitOption("Unit", "Description", "rkt apps target"),
		unit.NewUnitOption("Unit", "DefaultDependencies", "false"),
	}

	for i := range p.Manifest.Apps {
		ra := &p.Manifest.Apps[i]
		serviceName := ServiceUnitName(ra.Name)
		opts = append(opts, unit.NewUnitOption("Unit", "After", serviceName))
		opts = append(opts, unit.NewUnitOption("Unit", "Wants", serviceName))
	}

	unitsPath := filepath.Join(common.Stage1RootfsPath(p.Root), UnitsDir)
	file, err := os.OpenFile(filepath.Join(unitsPath, "default.target"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err = io.Copy(file, unit.Serialize(opts)); err != nil {
		return err
	}

	return nil
}

// WritePrepareAppTemplate writes service unit files for preparing the pod's applications
func WritePrepareAppTemplate(p *stage1commontypes.Pod) error {
	opts := []*unit.UnitOption{
		unit.NewUnitOption("Unit", "Description", "Prepare minimum environment for chrooted applications"),
		unit.NewUnitOption("Unit", "DefaultDependencies", "false"),
		unit.NewUnitOption("Unit", "OnFailureJobMode", "fail"),
		unit.NewUnitOption("Unit", "Requires", "systemd-journald.service"),
		unit.NewUnitOption("Unit", "After", "systemd-journald.service"),
		unit.NewUnitOption("Service", "Type", "oneshot"),
		unit.NewUnitOption("Service", "Restart", "no"),
		unit.NewUnitOption("Service", "ExecStart", "/prepare-app %I"),
		unit.NewUnitOption("Service", "User", "0"),
		unit.NewUnitOption("Service", "Group", "0"),
		unit.NewUnitOption("Service", "CapabilityBoundingSet", "CAP_SYS_ADMIN CAP_DAC_OVERRIDE"),
	}

	unitsPath := filepath.Join(common.Stage1RootfsPath(p.Root), UnitsDir)
	file, err := os.OpenFile(filepath.Join(unitsPath, "prepare-app@.service"), os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return errwrap.Wrap(errors.New("failed to create service unit file"), err)
	}
	defer file.Close()

	if _, err = io.Copy(file, unit.Serialize(opts)); err != nil {
		return errwrap.Wrap(errors.New("failed to write service unit file"), err)
	}

	return nil
}

func writeAppReaper(p *stage1commontypes.Pod, appName string) error {
	opts := []*unit.UnitOption{
		unit.NewUnitOption("Unit", "Description", fmt.Sprintf("%s Reaper", appName)),
		unit.NewUnitOption("Unit", "DefaultDependencies", "false"),
		unit.NewUnitOption("Unit", "StopWhenUnneeded", "yes"),
		unit.NewUnitOption("Unit", "Wants", "shutdown.service"),
		unit.NewUnitOption("Unit", "After", "shutdown.service"),
		unit.NewUnitOption("Unit", "Conflicts", "exit.target"),
		unit.NewUnitOption("Unit", "Conflicts", "halt.target"),
		unit.NewUnitOption("Unit", "Conflicts", "poweroff.target"),
		unit.NewUnitOption("Service", "RemainAfterExit", "yes"),
		unit.NewUnitOption("Service", "ExecStop", fmt.Sprintf("/reaper.sh %s", appName)),
	}

	unitsPath := filepath.Join(common.Stage1RootfsPath(p.Root), UnitsDir)
	file, err := os.OpenFile(filepath.Join(unitsPath, fmt.Sprintf("reaper-%s.service", appName)), os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return errwrap.Wrap(errors.New("failed to create service unit file"), err)
	}
	defer file.Close()

	if _, err = io.Copy(file, unit.Serialize(opts)); err != nil {
		return errwrap.Wrap(errors.New("failed to write service unit file"), err)
	}

	return nil
}

// SetJournalPermissions sets ACLs and permissions so the rkt group can access
// the pod's logs
func SetJournalPermissions(p *stage1commontypes.Pod) error {
	s1 := common.Stage1ImagePath(p.Root)

	rktgid, err := common.LookupGid(common.RktGroup)
	if err != nil {
		return fmt.Errorf("group %q not found", common.RktGroup)
	}

	journalPath := filepath.Join(s1, "rootfs", "var", "log", "journal")
	if err := os.MkdirAll(journalPath, os.FileMode(0755)); err != nil {
		return errwrap.Wrap(errors.New("error creating journal dir"), err)
	}

	a, err := acl.InitACL()
	if err != nil {
		return err
	}
	defer a.Free()

	if err := a.ParseACL(fmt.Sprintf("g:%d:r-x,m:r-x", rktgid)); err != nil {
		return errwrap.Wrap(errors.New("error parsing ACL string"), err)
	}

	if err := a.AddBaseEntries(journalPath); err != nil {
		return errwrap.Wrap(errors.New("error adding base ACL entries"), err)
	}

	if err := a.Valid(); err != nil {
		return err
	}

	if err := a.SetFileACLDefault(journalPath); err != nil {
		return errwrap.Wrap(fmt.Errorf("error setting default ACLs on %q", journalPath), err)
	}

	return nil
}

func generateGidArg(gid int, supplGid []int) string {
	arg := []string{strconv.Itoa(gid)}
	for _, sg := range supplGid {
		arg = append(arg, strconv.Itoa(sg))
	}
	return strings.Join(arg, ",")
}

// findHostPort returns the port number on the host that corresponds to an
// image manifest port identified by name
func findHostPort(pm schema.PodManifest, name types.ACName) uint {
	var port uint
	for _, p := range pm.Ports {
		if p.Name == name {
			port = p.HostPort
		}
	}
	return port
}

// generateSysusers generates systemd sysusers files for a given app so that
// corresponding entries in /etc/passwd and /etc/group are created in stage1.
// This is needed to use the "User=" and "Group=" options in the systemd
// service files of apps.
// If there're several apps defining the same UIDs/GIDs, systemd will take care
// of only generating one /etc/{passwd,group} entry
func generateSysusers(p *stage1commontypes.Pod, ra *schema.RuntimeApp, uid_ int, gid_ int, uidRange *uid.UidRange) error {
	var toShift []string

	app := ra.App
	appName := ra.Name

	sysusersDir := path.Join(common.Stage1RootfsPath(p.Root), "usr/lib/sysusers.d")
	toShift = append(toShift, sysusersDir)
	if err := os.MkdirAll(sysusersDir, 0755); err != nil {
		return err
	}

	gids := append(app.SupplementaryGIDs, gid_)

	// Create the Unix user and group
	var sysusersConf []string

	for _, g := range gids {
		groupname := "gen" + strconv.Itoa(g)
		sysusersConf = append(sysusersConf, fmt.Sprintf("g %s %d\n", groupname, g))
	}

	username := "gen" + strconv.Itoa(uid_)
	sysusersConf = append(sysusersConf, fmt.Sprintf("u %s %d \"%s\"\n", username, uid_, username))

	sysusersFile := path.Join(common.Stage1RootfsPath(p.Root), "usr/lib/sysusers.d", ServiceUnitName(appName)+".conf")
	toShift = append(toShift, sysusersFile)
	if err := ioutil.WriteFile(sysusersFile, []byte(strings.Join(sysusersConf, "\n")), 0640); err != nil {
		return err
	}

	if uidRange.Shift != 0 && uidRange.Count != 0 {
		for _, f := range toShift {
			if err := os.Chown(f, int(uidRange.Shift), int(uidRange.Shift)); err != nil {
				return err
			}
		}
	}

	return nil
}

// lookupPathInsideApp returns the path (relative to the app rootfs) of the
// given binary. It will look up on "paths" (also relative to the app rootfs)
// and evaluate possible symlinks to check if the resulting path is actually
// executable.
func lookupPathInsideApp(bin string, paths string, appRootfs string) (string, error) {
	pathsArr := filepath.SplitList(paths)
	var appPathsArr []string
	for _, p := range pathsArr {
		appPathsArr = append(appPathsArr, filepath.Join(appRootfs, p))
	}
	for _, path := range appPathsArr {
		binPath := filepath.Join(path, bin)
		binAbsPath, err := filepath.Abs(binPath)
		if err != nil {
			return "", fmt.Errorf("unable to find absolute path for %s", binPath)
		}
		relPath := strings.TrimPrefix(binAbsPath, appRootfs)
		d, err := os.Lstat(binAbsPath)
		if err != nil {
			continue
		}
		binRealPath, err := evaluateSymlinksInsideApp(appRootfs, relPath)
		if err != nil {
			return "", errwrap.Wrap(fmt.Errorf("could not evaluate path %v", binAbsPath), err)
		}
		binRealPath = filepath.Join(appRootfs, binRealPath)
		d, err = os.Stat(binRealPath)
		if err != nil {
			continue
		}
		// Check the executable bit, inspired by os.exec.LookPath()
		if m := d.Mode(); !m.IsDir() && m&0111 != 0 {
			// The real path is executable, return the path relative to the app
			return relPath, nil
		}
	}
	return "", fmt.Errorf("unable to find %q in %q", bin, paths)
}

// appSearchPaths returns a list of paths where we should search for
// non-absolute exec binaries
func appSearchPaths(p *stage1commontypes.Pod, appRootfs string, app types.App) []string {
	appEnv := app.Environment

	if imgPath, ok := appEnv.Get("PATH"); ok {
		return strings.Split(imgPath, ":")
	}

	// emulate exec(3) behavior, first check pod's directory and then the list
	// of directories returned by confstr(_CS_PATH). That's typically
	// "/bin:/usr/bin" so let's use that.
	return []string{appRootfs, "/bin", "/usr/bin"}
}

// findBinPath takes a binary path and returns a the absolute path of the
// binary relative to the app rootfs. This can be passed to ExecStart on the
// app's systemd service file directly.
func findBinPath(p *stage1commontypes.Pod, appName types.ACName, app types.App, bin string) (string, error) {
	absRoot, err := filepath.Abs(p.Root)
	if err != nil {
		return "", errwrap.Wrap(errors.New("could not get pod's root absolute path"), err)
	}

	appRootfs := common.AppRootfsPath(absRoot, appName)
	appPathDirs := appSearchPaths(p, appRootfs, app)
	appPath := strings.Join(appPathDirs, ":")

	binPath, err := lookupPathInsideApp(bin, appPath, appRootfs)
	if err != nil {
		return "", errwrap.Wrap(fmt.Errorf("error looking up %q", bin), err)
	}

	return strings.TrimPrefix(binPath, appRootfs), nil
}

// appToSystemd transforms the provided RuntimeApp+ImageManifest into systemd units
func appToSystemd(p *stage1commontypes.Pod, ra *schema.RuntimeApp, interactive bool, flavor string, privateUsers string) error {
	app := ra.App
	appName := ra.Name
	imgName := p.AppNameToImageName(appName)

	if len(app.Exec) == 0 {
		return fmt.Errorf(`image %q has an empty "exec" (try --exec=BINARY)`, imgName)
	}

	workDir := "/"
	if app.WorkingDirectory != "" {
		workDir = app.WorkingDirectory
	}

	env := app.Environment

	env.Set("AC_APP_NAME", appName.String())
	if p.MetadataServiceURL != "" {
		env.Set("AC_METADATA_URL", p.MetadataServiceURL)
	}

	// TODO(iaguis): make enterexec use the same format as systemd
	envFilePathSystemd := EnvFilePathSystemd(p.Root, appName)
	envFilePathEnterexec := EnvFilePathEnterexec(p.Root, appName)

	uidRange := uid.NewBlankUidRange()
	if err := uidRange.Deserialize([]byte(privateUsers)); err != nil {
		return err
	}

	if err := writeEnvFile(p, env, appName, uidRange, '\n', envFilePathSystemd); err != nil {
		return errwrap.Wrap(errors.New("unable to write environment file for systemd"), err)
	}

	if err := writeEnvFile(p, env, appName, uidRange, '\000', envFilePathEnterexec); err != nil {
		return errwrap.Wrap(errors.New("unable to write environment file for enterexec"), err)
	}

	u, g, err := parseUserGroup(p, ra, uidRange)
	if err != nil {
		return err
	}

	if err := generateSysusers(p, ra, u, g, uidRange); err != nil {
		return errwrap.Wrap(errors.New("unable to generate sysusers"), err)
	}

	binPath := app.Exec[0]
	if !filepath.IsAbs(binPath) {
		binPath, err = findBinPath(p, appName, *app, binPath)
		if err != nil {
			return err
		}
	}

	var supplementaryGroups []string
	for _, g := range app.SupplementaryGIDs {
		supplementaryGroups = append(supplementaryGroups, strconv.Itoa(g))
	}

	// TODO: read the RemoveSet as well. See
	// https://github.com/coreos/rkt/issues/2348#issuecomment-211796840
	capabilities := append(app.Isolators, appDefaultCapabilities.AsIsolator())
	capabilitiesStr := GetAppCapabilities(capabilities)

	execStart := append([]string{binPath}, app.Exec[1:]...)
	execStartString := quoteExec(execStart)
	opts := []*unit.UnitOption{
		unit.NewUnitOption("Unit", "Description", fmt.Sprintf("Application=%v Image=%v", appName, imgName)),
		unit.NewUnitOption("Unit", "DefaultDependencies", "false"),
		unit.NewUnitOption("Unit", "Wants", fmt.Sprintf("reaper-%s.service", appName)),
		unit.NewUnitOption("Service", "Restart", "no"),
		unit.NewUnitOption("Service", "ExecStart", execStartString),
		unit.NewUnitOption("Service", "RootDirectory", common.RelAppRootfsPath(appName)),
		unit.NewUnitOption("Service", "WorkingDirectory", workDir),
		unit.NewUnitOption("Service", "EnvironmentFile", RelEnvFilePathSystemd(appName)),
		unit.NewUnitOption("Service", "User", strconv.Itoa(u)),
		unit.NewUnitOption("Service", "Group", strconv.Itoa(g)),
		unit.NewUnitOption("Service", "SupplementaryGroups", strings.Join(supplementaryGroups, " ")),
		unit.NewUnitOption("Service", "CapabilityBoundingSet", strings.Join(capabilitiesStr, " ")),
	}

	if interactive {
		opts = append(opts, unit.NewUnitOption("Service", "StandardInput", "tty"))
		opts = append(opts, unit.NewUnitOption("Service", "StandardOutput", "tty"))
		opts = append(opts, unit.NewUnitOption("Service", "StandardError", "tty"))
	} else {
		opts = append(opts, unit.NewUnitOption("Service", "StandardOutput", "journal+console"))
		opts = append(opts, unit.NewUnitOption("Service", "StandardError", "journal+console"))
		opts = append(opts, unit.NewUnitOption("Service", "SyslogIdentifier", filepath.Base(app.Exec[0])))
	}

	// When an app fails, we shut down the pod
	opts = append(opts, unit.NewUnitOption("Unit", "OnFailure", "halt.target"))

	for _, eh := range app.EventHandlers {
		var typ string
		switch eh.Name {
		case "pre-start":
			typ = "ExecStartPre"
		case "post-stop":
			typ = "ExecStopPost"
		default:
			return fmt.Errorf("unrecognized eventHandler: %v", eh.Name)
		}
		exec := quoteExec(eh.Exec)
		opts = append(opts, unit.NewUnitOption("Service", typ, exec))
	}

	// Some pre-start jobs take a long time, set the timeout to 0
	opts = append(opts, unit.NewUnitOption("Service", "TimeoutStartSec", "0"))

	var saPorts []types.Port
	for _, p := range app.Ports {
		if p.SocketActivated {
			saPorts = append(saPorts, p)
		}
	}

	for _, i := range app.Isolators {
		switch v := i.Value().(type) {
		case *types.ResourceMemory:
			opts, err = cgroup.MaybeAddIsolator(opts, "memory", v.Limit())
			if err != nil {
				return err
			}
		case *types.ResourceCPU:
			opts, err = cgroup.MaybeAddIsolator(opts, "cpu", v.Limit())
			if err != nil {
				return err
			}
		}
	}

	if len(saPorts) > 0 {
		sockopts := []*unit.UnitOption{
			unit.NewUnitOption("Unit", "Description", fmt.Sprintf("Application=%v Image=%v %s", appName, imgName, "socket-activated ports")),
			unit.NewUnitOption("Unit", "DefaultDependencies", "false"),
			unit.NewUnitOption("Socket", "BindIPv6Only", "both"),
			unit.NewUnitOption("Socket", "Service", ServiceUnitName(appName)),
		}

		for _, sap := range saPorts {
			var proto string
			switch sap.Protocol {
			case "tcp":
				proto = "ListenStream"
			case "udp":
				proto = "ListenDatagram"
			default:
				return fmt.Errorf("unrecognized protocol: %v", sap.Protocol)
			}
			// We find the host port for the pod's port and use that in the
			// socket unit file.
			// This is so because systemd inside the pod will match based on
			// the socket port number, and since the socket was created on the
			// host, it will have the host port number.
			port := findHostPort(*p.Manifest, sap.Name)
			if port == 0 {
				log.Printf("warning: no --port option for socket-activated port %q, assuming port %d as specified in the manifest", sap.Name, sap.Port)
				port = sap.Port
			}
			sockopts = append(sockopts, unit.NewUnitOption("Socket", proto, fmt.Sprintf("%v", port)))
		}

		file, err := os.OpenFile(SocketUnitPath(p.Root, appName), os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			return errwrap.Wrap(errors.New("failed to create socket file"), err)
		}
		defer file.Close()

		if _, err = io.Copy(file, unit.Serialize(sockopts)); err != nil {
			return errwrap.Wrap(errors.New("failed to write socket unit file"), err)
		}

		if err = os.Symlink(path.Join("..", SocketUnitName(appName)), SocketWantPath(p.Root, appName)); err != nil {
			return errwrap.Wrap(errors.New("failed to link socket want"), err)
		}

		opts = append(opts, unit.NewUnitOption("Unit", "Requires", SocketUnitName(appName)))
	}

	opts = append(opts, unit.NewUnitOption("Unit", "Requires", InstantiatedPrepareAppUnitName(appName)))
	opts = append(opts, unit.NewUnitOption("Unit", "After", InstantiatedPrepareAppUnitName(appName)))

	opts = append(opts, unit.NewUnitOption("Unit", "Requires", "sysusers.service"))
	opts = append(opts, unit.NewUnitOption("Unit", "After", "sysusers.service"))

	file, err := os.OpenFile(ServiceUnitPath(p.Root, appName), os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return errwrap.Wrap(errors.New("failed to create service unit file"), err)
	}
	defer file.Close()

	if _, err = io.Copy(file, unit.Serialize(opts)); err != nil {
		return errwrap.Wrap(errors.New("failed to write service unit file"), err)
	}

	if err = os.Symlink(path.Join("..", ServiceUnitName(appName)), ServiceWantPath(p.Root, appName)); err != nil {
		return errwrap.Wrap(errors.New("failed to link service want"), err)
	}

	if err = writeAppReaper(p, appName.String()); err != nil {
		return errwrap.Wrap(fmt.Errorf("failed to write app %q reaper service", appName), err)
	}

	return nil
}

// parseUserGroup parses the User and Group fields of an App and returns its
// UID and GID.
// The User and Group fields accept several formats:
//   1. the hardcoded string "root"
//   2. a path
//   3. a number
//   4. a name in reference to /etc/{group,passwd} in the image
// See https://github.com/appc/spec/blob/master/spec/aci.md#image-manifest-schema
func parseUserGroup(p *stage1commontypes.Pod, ra *schema.RuntimeApp, uidRange *uid.UidRange) (int, int, error) {
	app := ra.App
	appName := ra.Name

	var uid_, gid_ int
	var err error

	switch {
	case app.User == "root":
		uid_ = 0
	case strings.HasPrefix(app.User, "/"):
		var stat syscall.Stat_t
		if err = syscall.Lstat(filepath.Join(common.AppRootfsPath(p.Root, appName),
			app.User), &stat); err != nil {
			return -1, -1, errwrap.Wrap(fmt.Errorf("unable to get uid from file %q",
				app.User), err)
		}
		uidReal, _, err := uidRange.UnshiftRange(stat.Uid, 0)
		if err != nil {
			return -1, -1, errwrap.Wrap(errors.New("unable to determine real uid"), err)
		}
		uid_ = int(uidReal)
	default:
		uid_, err = strconv.Atoi(app.User)
		if err != nil {
			uid_, err = passwd.LookupUidFromFile(app.User,
				filepath.Join(common.AppRootfsPath(p.Root, appName), "etc/passwd"))
			if err != nil {
				return -1, -1, errwrap.Wrap(fmt.Errorf("cannot lookup user %q", app.User), err)
			}
		}
	}

	switch {
	case app.Group == "root":
		gid_ = 0
	case strings.HasPrefix(app.Group, "/"):
		var stat syscall.Stat_t
		if err = syscall.Lstat(filepath.Join(common.AppRootfsPath(p.Root, appName),
			app.Group), &stat); err != nil {
			return -1, -1, errwrap.Wrap(fmt.Errorf("unable to get gid from file %q",
				app.Group), err)
		}
		_, gidReal, err := uidRange.UnshiftRange(0, stat.Gid)
		if err != nil {
			return -1, -1, errwrap.Wrap(errors.New("unable to determine real gid"), err)
		}
		gid_ = int(gidReal)
	default:
		gid_, err = strconv.Atoi(app.Group)
		if err != nil {
			gid_, err = group.LookupGidFromFile(app.Group,
				filepath.Join(common.AppRootfsPath(p.Root, appName), "etc/group"))
			if err != nil {
				return -1, -1, errwrap.Wrap(fmt.Errorf("cannot lookup group %q", app.Group), err)
			}
		}
	}

	return uid_, gid_, nil
}

func writeShutdownService(p *stage1commontypes.Pod) error {
	flavor, systemdVersion, err := GetFlavor(p)
	if err != nil {
		return err
	}

	opts := []*unit.UnitOption{
		unit.NewUnitOption("Unit", "Description", "Pod shutdown"),
		unit.NewUnitOption("Unit", "AllowIsolate", "true"),
		unit.NewUnitOption("Unit", "StopWhenUnneeded", "yes"),
		unit.NewUnitOption("Unit", "DefaultDependencies", "false"),
		unit.NewUnitOption("Service", "RemainAfterExit", "yes"),
	}

	shutdownVerb := "exit"
	// systemd <v227 doesn't allow the "exit" verb when running as PID 1, so
	// use "halt".
	// If systemdVersion is 0 it means it couldn't be guessed, assume it's new
	// enough for "systemctl exit".
	// This can happen, for example, when building rkt with:
	//
	// ./configure --with-stage1-flavors=src --with-stage1-systemd-version=master
	//
	// The patches for the "exit" verb are backported to the "coreos" flavor, so
	// don't rely on the systemd version on the "coreos" flavor.
	if flavor != "coreos" && systemdVersion != 0 && systemdVersion < 227 {
		shutdownVerb = "halt"
	}

	opts = append(opts, unit.NewUnitOption("Service", "ExecStop", fmt.Sprintf("/usr/bin/systemctl --force %s", shutdownVerb)))

	unitsPath := filepath.Join(common.Stage1RootfsPath(p.Root), UnitsDir)
	file, err := os.OpenFile(filepath.Join(unitsPath, "shutdown.service"), os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return errwrap.Wrap(errors.New("failed to create unit file"), err)
	}
	defer file.Close()

	if _, err = io.Copy(file, unit.Serialize(opts)); err != nil {
		return errwrap.Wrap(errors.New("failed to write unit file"), err)
	}

	return nil
}

// writeEnvFile creates an environment file for given app name, the minimum
// required environment variables by the appc spec will be set to sensible
// defaults here if they're not provided by env.
func writeEnvFile(p *stage1commontypes.Pod, env types.Environment, appName types.ACName, uidRange *uid.UidRange, separator byte, envFilePath string) error {
	ef := bytes.Buffer{}

	for dk, dv := range defaultEnv {
		if _, exists := env.Get(dk); !exists {
			fmt.Fprintf(&ef, "%s=%s%c", dk, dv, separator)
		}
	}

	for _, e := range env {
		fmt.Fprintf(&ef, "%s=%s%c", e.Name, e.Value, separator)
	}

	if err := ioutil.WriteFile(envFilePath, ef.Bytes(), 0644); err != nil {
		return err
	}

	if uidRange.Shift != 0 && uidRange.Count != 0 {
		if err := os.Chown(envFilePath, int(uidRange.Shift), int(uidRange.Shift)); err != nil {
			return err
		}
	}

	return nil
}

// PodToSystemd creates the appropriate systemd service unit files for
// all the constituent apps of the Pod
func PodToSystemd(p *stage1commontypes.Pod, interactive bool, flavor string, privateUsers string) error {
	for i := range p.Manifest.Apps {
		ra := &p.Manifest.Apps[i]
		if err := appToSystemd(p, ra, interactive, flavor, privateUsers); err != nil {
			return errwrap.Wrap(fmt.Errorf("failed to transform app %q into systemd service", ra.Name), err)
		}
	}

	if err := writeShutdownService(p); err != nil {
		return errwrap.Wrap(errors.New("failed to write shutdown service"), err)
	}

	return nil
}

// evaluateSymlinksInsideApp tries to resolve symlinks within the path.
// It returns the actual path relative to the pod for the given path.
func evaluateSymlinksInsideApp(appRootfs, path string) (string, error) {
	link := appRootfs

	paths := strings.Split(path, "/")
	for i, p := range paths {
		next := filepath.Join(link, p)

		if !strings.HasPrefix(next, appRootfs) {
			return "", fmt.Errorf("path escapes app's root: %q", path)
		}

		fi, err := os.Lstat(next)
		if err != nil {
			if os.IsNotExist(err) {
				link = filepath.Join(append([]string{link}, paths[i:]...)...)
				break
			}
			return "", err
		}

		if fi.Mode()&os.ModeType != os.ModeSymlink {
			link = filepath.Join(link, p)
			continue
		}

		// Evaluate the symlink.
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}

		if filepath.IsAbs(target) {
			link = filepath.Join(appRootfs, target)
		} else {
			link = filepath.Join(link, target)
		}

		if !strings.HasPrefix(link, appRootfs) {
			return "", fmt.Errorf("symlink %q escapes app's root with value %q", next, target)
		}
	}

	return strings.TrimPrefix(link, appRootfs), nil
}

// appToNspawnArgs transforms the given app manifest, with the given associated
// app name, into a subset of applicable systemd-nspawn argument
func appToNspawnArgs(p *stage1commontypes.Pod, ra *schema.RuntimeApp) ([]string, error) {
	var args []string
	appName := ra.Name
	app := ra.App

	sharedVolPath := common.SharedVolumesPath(p.Root)
	if err := os.MkdirAll(sharedVolPath, SharedVolPerm); err != nil {
		return nil, errwrap.Wrap(errors.New("could not create shared volumes directory"), err)
	}
	if err := os.Chmod(sharedVolPath, SharedVolPerm); err != nil {
		return nil, errwrap.Wrap(fmt.Errorf("could not change permissions of %q", sharedVolPath), err)
	}

	vols := make(map[types.ACName]types.Volume)
	for _, v := range p.Manifest.Volumes {
		vols[v.Name] = v
	}

	imageManifest := p.Images[appName.String()]
	mounts := GenerateMounts(ra, vols, imageManifest)
	for _, m := range mounts {
		vol := vols[m.Volume]

		shPath := filepath.Join(sharedVolPath, vol.Name.String())

		absRoot, err := filepath.Abs(p.Root) // Absolute path to the pod's rootfs.
		if err != nil {
			return nil, errwrap.Wrap(errors.New("could not get pod's root absolute path"), err)
		}

		appRootfs := common.AppRootfsPath(absRoot, appName)

		// TODO(yifan): This is a temporary fix for systemd-nspawn not handling symlink mounts well.
		// Could be removed when https://github.com/systemd/systemd/issues/2860 is resolved, and systemd
		// version is bumped.
		mntPath, err := evaluateSymlinksInsideApp(appRootfs, m.Path)
		if err != nil {
			return nil, errwrap.Wrap(fmt.Errorf("could not evaluate path %v", m.Path), err)
		}
		mntAbsPath := filepath.Join(appRootfs, mntPath)

		if err := PrepareMountpoints(shPath, mntAbsPath, &vol, m.dockerImplicit); err != nil {
			return nil, err
		}

		opt := make([]string, 4)

		if IsMountReadOnly(vol, app.MountPoints) {
			opt[0] = "--bind-ro="
		} else {
			opt[0] = "--bind="
		}

		switch vol.Kind {
		case "host":
			opt[1] = vol.Source
		case "empty":
			opt[1] = filepath.Join(common.SharedVolumesPath(absRoot), vol.Name.String())
		default:
			return nil, fmt.Errorf(`invalid volume kind %q. Must be one of "host" or "empty"`, vol.Kind)
		}
		opt[2] = ":"
		opt[3] = filepath.Join(common.RelAppRootfsPath(appName), mntPath)
		args = append(args, strings.Join(opt, ""))
	}

	capList := strings.Join(GetAppCapabilities(app.Isolators), ",")
	args = append(args, "--capability="+capList)

	return args, nil
}

// PodToNspawnArgs renders a prepared Pod as a systemd-nspawn
// argument list ready to be executed
func PodToNspawnArgs(p *stage1commontypes.Pod) ([]string, error) {
	args := []string{
		"--uuid=" + p.UUID.String(),
		"--machine=" + GetMachineID(p),
		"--directory=" + common.Stage1RootfsPath(p.Root),
	}

	for i := range p.Manifest.Apps {
		aa, err := appToNspawnArgs(p, &p.Manifest.Apps[i])
		if err != nil {
			return nil, err
		}
		args = append(args, aa...)
	}

	return args, nil
}

// GetFlavor populates a flavor string based on the flavor itself and respectively the systemd version
// If the systemd version couldn't be guessed, it will be set to 0.
func GetFlavor(p *stage1commontypes.Pod) (flavor string, systemdVersion int, err error) {
	flavor, err = os.Readlink(filepath.Join(common.Stage1RootfsPath(p.Root), "flavor"))
	if err != nil {
		return "", -1, errwrap.Wrap(errors.New("unable to determine stage1 flavor"), err)
	}

	if flavor == "host" {
		// This flavor does not contain systemd, parse "systemctl --version"
		systemctlBin, err := common.LookupPath("systemctl", os.Getenv("PATH"))
		if err != nil {
			return "", -1, err
		}

		systemdVersion, err := common.SystemdVersion(systemctlBin)
		if err != nil {
			return "", -1, errwrap.Wrap(errors.New("error finding systemctl version"), err)
		}

		return flavor, systemdVersion, nil
	}

	systemdVersionBytes, err := ioutil.ReadFile(filepath.Join(common.Stage1RootfsPath(p.Root), "systemd-version"))
	if err != nil {
		return "", -1, errwrap.Wrap(errors.New("unable to determine stage1's systemd version"), err)
	}
	systemdVersionString := strings.Trim(string(systemdVersionBytes), " \n")

	// systemdVersionString is either a tag name or a branch name. If it's a
	// tag name it's of the form "v229", remove the first character to get the
	// number.
	systemdVersion, err = strconv.Atoi(systemdVersionString[1:])
	if err != nil {
		// If we get a syntax error, it means the parsing of the version string
		// of the form "v229" failed, set it to 0 to indicate we couldn't guess
		// it.
		if e, ok := err.(*strconv.NumError); ok && e.Err == strconv.ErrSyntax {
			systemdVersion = 0
		} else {
			return "", -1, errwrap.Wrap(errors.New("error parsing stage1's systemd version"), err)
		}
	}
	return flavor, systemdVersion, nil
}

// GetAppHashes returns a list of hashes of the apps in this pod
func GetAppHashes(p *stage1commontypes.Pod) []types.Hash {
	var names []types.Hash
	for _, a := range p.Manifest.Apps {
		names = append(names, a.Image.ID)
	}

	return names
}

// GetMachineID returns the machine id string of the pod to be passed to
// systemd-nspawn
func GetMachineID(p *stage1commontypes.Pod) string {
	return "rkt-" + p.UUID.String()
}

// GetAppCapabilities is a filter which returns a string slice of valid Linux capabilities
// It requires list of available isolators
func GetAppCapabilities(isolators types.Isolators) []string {
	var caps []string

	for _, isolator := range isolators {
		if capSet, ok := isolator.Value().(types.LinuxCapabilitiesSet); ok &&
			isolator.Name == types.LinuxCapabilitiesRetainSetName {
			caps = append(caps, parseLinuxCapabilitiesSet(capSet)...)
		}
	}
	return caps
}

// parseLinuxCapabilitySet parses a LinuxCapabilitiesSet into string slice
func parseLinuxCapabilitiesSet(capSet types.LinuxCapabilitiesSet) []string {
	var capsStr []string
	for _, cap := range capSet.Set() {
		capsStr = append(capsStr, string(cap))
	}
	return capsStr
}
