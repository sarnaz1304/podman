//go:build amd64 || arm64

package ignition

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"

	"github.com/containers/podman/v5/pkg/machine/define"
	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/sirupsen/logrus"
)

/*
	If this file gets too nuts, we can perhaps use existing go code
	to create ignition files.  At this point, the file is so simple
	that I chose to use structs and not import any code as I was
	concerned (unsubstantiated) about too much bloat coming in.

	https://github.com/openshift/machine-config-operator/blob/master/pkg/server/server.go
*/

const (
	PodmanDockerTmpConfPath = "/etc/tmpfiles.d/podman-docker.conf"
	DefaultIgnitionUserName = "core"
)

// Convenience function to convert int to ptr
func IntToPtr(i int) *int {
	return &i
}

// Convenience function to convert string to ptr
func StrToPtr(s string) *string {
	return &s
}

// Convenience function to convert bool to ptr
func BoolToPtr(b bool) *bool {
	return &b
}

func GetNodeUsr(usrName string) NodeUser {
	return NodeUser{Name: &usrName}
}

func GetNodeGrp(grpName string) NodeGroup {
	return NodeGroup{Name: &grpName}
}

type DynamicIgnition struct {
	Name       string
	Key        string
	TimeZone   string
	UID        int
	VMName     string
	VMType     define.VMType
	WritePath  string
	Cfg        Config
	Rootful    bool
	NetRecover bool
}

func (ign *DynamicIgnition) Write() error {
	b, err := json.Marshal(ign.Cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(ign.WritePath, b, 0644)
}

func (ign *DynamicIgnition) getUsers() []PasswdUser {
	var (
		users []PasswdUser
	)

	isCoreUser := ign.Name == DefaultIgnitionUserName

	// if we are not using the 'core' user, we need to tell ignition to
	// not add it
	if !isCoreUser {
		coreUser := PasswdUser{
			Name:        DefaultIgnitionUserName,
			ShouldExist: BoolToPtr(false),
		}
		users = append(users, coreUser)
	}

	// Adding the user
	user := PasswdUser{
		Name:              ign.Name,
		SSHAuthorizedKeys: []SSHAuthorizedKey{SSHAuthorizedKey(ign.Key)},
		UID:               IntToPtr(ign.UID),
	}

	// If we are not using the core user, we need to make the user part
	// of the following groups
	if !isCoreUser {
		user.Groups = []Group{
			Group("sudo"),
			Group("adm"),
			Group("wheel"),
			Group("systemd-journal")}
	}

	// set root SSH key
	root := PasswdUser{
		Name:              "root",
		SSHAuthorizedKeys: []SSHAuthorizedKey{SSHAuthorizedKey(ign.Key)},
	}
	// add them all in
	users = append(users, user, root)

	return users
}

// GenerateIgnitionConfig
func (ign *DynamicIgnition) GenerateIgnitionConfig() error {
	if len(ign.Name) < 1 {
		ign.Name = DefaultIgnitionUserName
	}
	ignVersion := Ignition{
		Version: "3.2.0",
	}
	ignPassword := Passwd{
		Users: ign.getUsers(),
	}

	ignStorage := Storage{
		Directories: getDirs(ign.Name),
		Files:       getFiles(ign.Name, ign.UID, ign.Rootful, ign.VMType, ign.NetRecover),
		Links:       getLinks(ign.Name),
	}

	// Add or set the time zone for the machine
	if len(ign.TimeZone) > 0 {
		var (
			err error
			tz  string
		)
		// local means the same as the host
		// look up where it is pointing to on the host
		if ign.TimeZone == "local" {
			tz, err = getLocalTimeZone()
			if err != nil {
				return err
			}
		} else {
			tz = ign.TimeZone
		}
		tzLink := Link{
			Node: Node{
				Group:     GetNodeGrp("root"),
				Path:      "/etc/localtime",
				Overwrite: BoolToPtr(false),
				User:      GetNodeUsr("root"),
			},
			LinkEmbedded1: LinkEmbedded1{
				Hard: BoolToPtr(false),
				// We always want this value in unix form (/path/to/something) because this is being
				// set in the machine OS (always Linux).  However, filepath.join on windows will use a "\\"
				// separator; therefore we use ToSlash to convert the path to unix style
				Target: filepath.ToSlash(filepath.Join("/usr/share/zoneinfo", tz)),
			},
		}
		ignStorage.Links = append(ignStorage.Links, tzLink)
	}

	// Enables automatic login on the console;
	// there's no security concerns here, and this makes debugging easier.
	// xref https://docs.fedoraproject.org/en-US/fedora-coreos/tutorial-autologin/
	var autologinDropin = `[Service]
ExecStart=
ExecStart=-/usr/sbin/agetty --autologin root --noclear %I $TERM
`

	ignSystemd := Systemd{
		Units: []Unit{
			{
				Enabled: BoolToPtr(true),
				Name:    "podman.socket",
			},
			{
				Enabled: BoolToPtr(false),
				Name:    "docker.service",
				Mask:    BoolToPtr(true),
			},
			{
				Enabled: BoolToPtr(false),
				Name:    "docker.socket",
				Mask:    BoolToPtr(true),
			},
			{
				// Disable auto-updating of fcos images
				// https://github.com/containers/podman/issues/20122
				Enabled: BoolToPtr(false),
				Name:    "zincati.service",
			},
			{
				Name: "serial-getty@.service",
				Dropins: []Dropin{
					{
						Name:     "10-autologin.conf",
						Contents: &autologinDropin,
					},
				},
			},
			{
				Name: "getty@.service",
				Dropins: []Dropin{
					{
						Name:     "10-autologin.conf",
						Contents: &autologinDropin,
					},
				},
			},
		},
	}

	if ign.NetRecover {
		contents, err := GetNetRecoveryUnitFile().ToString()
		if err != nil {
			return err
		}

		recoveryUnit := Unit{
			Enabled:  BoolToPtr(true),
			Name:     "net-health-recovery.service",
			Contents: &contents,
		}
		ignSystemd.Units = append(ignSystemd.Units, recoveryUnit)
	}

	// Only after all checks are done
	// it's ready create the ingConfig
	ign.Cfg = Config{
		Ignition: ignVersion,
		Passwd:   ignPassword,
		Storage:  ignStorage,
		Systemd:  ignSystemd,
	}
	return nil
}

func getDirs(usrName string) []Directory {
	// Ignition has a bug/feature? where if you make a series of dirs
	// in one swoop, then the leading dirs are creates as root.
	newDirs := []string{
		"/home/" + usrName + "/.config",
		"/home/" + usrName + "/.config/containers",
		"/home/" + usrName + "/.config/systemd",
		"/home/" + usrName + "/.config/systemd/user",
		"/home/" + usrName + "/.config/systemd/user/default.target.wants",
	}
	var (
		dirs = make([]Directory, len(newDirs))
	)
	for i, d := range newDirs {
		newDir := Directory{
			Node: Node{
				Group: GetNodeGrp(usrName),
				Path:  d,
				User:  GetNodeUsr(usrName),
			},
			DirectoryEmbedded1: DirectoryEmbedded1{Mode: IntToPtr(0755)},
		}
		dirs[i] = newDir
	}

	// Issue #11489: make sure that we can inject a custom registries.conf
	// file on the system level to force a single search registry.
	// The remote client does not yet support prompting for short-name
	// resolution, so we enforce a single search registry (i.e., docker.io)
	// as a workaround.
	dirs = append(dirs, Directory{
		Node: Node{
			Group: GetNodeGrp("root"),
			Path:  "/etc/containers/registries.conf.d",
			User:  GetNodeUsr("root"),
		},
		DirectoryEmbedded1: DirectoryEmbedded1{Mode: IntToPtr(0755)},
	})

	// The directory is used by envset-fwcfg.service
	// for propagating environment variables that got
	// from a host
	dirs = append(dirs, Directory{
		Node: Node{
			Group: GetNodeGrp("root"),
			Path:  "/etc/systemd/system.conf.d",
			User:  GetNodeUsr("root"),
		},
		DirectoryEmbedded1: DirectoryEmbedded1{Mode: IntToPtr(0755)},
	}, Directory{
		Node: Node{
			Group: GetNodeGrp("root"),
			Path:  "/etc/environment.d",
			User:  GetNodeUsr("root"),
		},
		DirectoryEmbedded1: DirectoryEmbedded1{Mode: IntToPtr(0755)},
	})

	return dirs
}

func getFiles(usrName string, uid int, rootful bool, vmtype define.VMType, netRecover bool) []File {
	files := make([]File, 0)

	lingerExample := parser.NewUnitFile()
	lingerExample.Add("Unit", "Description", "A systemd user unit demo")
	lingerExample.Add("Unit", "After", "network-online.target")
	lingerExample.Add("Unit", "Wants", "network-online.target podman.socket")
	lingerExample.Add("Service", "ExecStart", "/usr/bin/sleep infinity")
	lingerExampleFile, err := lingerExample.ToString()
	if err != nil {
		logrus.Warnf(err.Error())
	}

	containers := `[containers]
netns="bridge"
pids_limit=0
`
	// Set deprecated machine_enabled until podman package on fcos is
	// current enough to no longer require it
	rootContainers := `[engine]
machine_enabled=true
`

	delegateConf := `[Service]
Delegate=memory pids cpu io
`
	// Prevent subUID from clashing with actual UID
	subUID := 100000
	subUIDs := 1000000
	if uid >= subUID && uid < (subUID+subUIDs) {
		subUID = uid + 1
	}
	etcSubUID := fmt.Sprintf(`%s:%d:%d`, usrName, subUID, subUIDs)

	// Add a fake systemd service to get the user socket rolling
	files = append(files, File{
		Node: Node{
			Group: GetNodeGrp(usrName),
			Path:  "/home/" + usrName + "/.config/systemd/user/linger-example.service",
			User:  GetNodeUsr(usrName),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: EncodeDataURLPtr(lingerExampleFile),
			},
			Mode: IntToPtr(0744),
		},
	})

	// Set containers.conf up for core user to use networks
	// by default
	files = append(files, File{
		Node: Node{
			Group: GetNodeGrp(usrName),
			Path:  "/home/" + usrName + "/.config/containers/containers.conf",
			User:  GetNodeUsr(usrName),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: EncodeDataURLPtr(containers),
			},
			Mode: IntToPtr(0744),
		},
	})
	// Set up /etc/subuid and /etc/subgid
	for _, sub := range []string{"/etc/subuid", "/etc/subgid"} {
		files = append(files, File{
			Node: Node{
				Group:     GetNodeGrp("root"),
				Path:      sub,
				User:      GetNodeUsr("root"),
				Overwrite: BoolToPtr(true),
			},
			FileEmbedded1: FileEmbedded1{
				Append: nil,
				Contents: Resource{
					Source: EncodeDataURLPtr(etcSubUID),
				},
				Mode: IntToPtr(0744),
			},
		})
	}

	// Set delegate.conf so cpu,io subsystem is delegated to non-root users as well for cgroupv2
	// by default
	files = append(files, File{
		Node: Node{
			Group: GetNodeGrp("root"),
			Path:  "/etc/systemd/system/user@.service.d/delegate.conf",
			User:  GetNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: EncodeDataURLPtr(delegateConf),
			},
			Mode: IntToPtr(0644),
		},
	})

	// Add a file into linger
	files = append(files, File{
		Node: Node{
			Group: GetNodeGrp(usrName),
			Path:  "/var/lib/systemd/linger/core",
			User:  GetNodeUsr(usrName),
		},
		FileEmbedded1: FileEmbedded1{Mode: IntToPtr(0644)},
	})

	// Set deprecated machine_enabled to true to indicate we're in a VM
	files = append(files, File{
		Node: Node{
			Group: GetNodeGrp("root"),
			Path:  "/etc/containers/containers.conf",
			User:  GetNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: EncodeDataURLPtr(rootContainers),
			},
			Mode: IntToPtr(0644),
		},
	})

	// Set machine marker file to indicate podman is in a qemu based machine
	files = append(files, File{
		Node: Node{
			Group: GetNodeGrp("root"),
			Path:  "/etc/containers/podman-machine",
			User:  GetNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: EncodeDataURLPtr(fmt.Sprintf("%s\n", vmtype.String())),
			},
			Mode: IntToPtr(0644),
		},
	})

	// Increase the number of inotify instances.
	files = append(files, File{
		Node: Node{
			Group: GetNodeGrp("root"),
			Path:  "/etc/sysctl.d/10-inotify-instances.conf",
			User:  GetNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: EncodeDataURLPtr("fs.inotify.max_user_instances=524288\n"),
			},
			Mode: IntToPtr(0644),
		},
	})

	// Issue #11489: make sure that we can inject a custom registries.conf
	// file on the system level to force a single search registry.
	// The remote client does not yet support prompting for short-name
	// resolution, so we enforce a single search registry (i.e., docker.io)
	// as a workaround.
	files = append(files, File{
		Node: Node{
			Group: GetNodeGrp("root"),
			Path:  "/etc/containers/registries.conf.d/999-podman-machine.conf",
			User:  GetNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: EncodeDataURLPtr("unqualified-search-registries=[\"docker.io\"]\n"),
			},
			Mode: IntToPtr(0644),
		},
	})

	files = append(files, File{
		Node: Node{
			Path: PodmanDockerTmpConfPath,
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			// Create a symlink from the docker socket to the podman socket.
			Contents: Resource{
				Source: EncodeDataURLPtr(GetPodmanDockerTmpConfig(uid, rootful, true)),
			},
			Mode: IntToPtr(0644),
		},
	})

	setDockerHost := `export DOCKER_HOST="unix://$(podman info -f "{{.Host.RemoteSocket.Path}}")"
`

	files = append(files, File{
		Node: Node{
			Group: GetNodeGrp("root"),
			Path:  "/etc/profile.d/docker-host.sh",
			User:  GetNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: EncodeDataURLPtr(setDockerHost),
			},
			Mode: IntToPtr(0644),
		},
	})

	// get certs for current user
	userHome, err := os.UserHomeDir()
	if err != nil {
		logrus.Warnf("Unable to copy certs via ignition %s", err.Error())
		return files
	}

	certFiles := getCerts(filepath.Join(userHome, ".config/containers/certs.d"), true)
	files = append(files, certFiles...)

	certFiles = getCerts(filepath.Join(userHome, ".config/docker/certs.d"), true)
	files = append(files, certFiles...)

	sslCertFileName, ok := os.LookupEnv(sslCertFile)
	if ok {
		if _, err := os.Stat(sslCertFileName); err == nil {
			certFiles = getCerts(sslCertFileName, false)
			files = append(files, certFiles...)
		} else {
			logrus.Warnf("Invalid path in %s: %q", sslCertFile, err)
		}
	}

	sslCertDirName, ok := os.LookupEnv(sslCertDir)
	if ok {
		if _, err := os.Stat(sslCertDirName); err == nil {
			certFiles = getCerts(sslCertDirName, true)
			files = append(files, certFiles...)
		} else {
			logrus.Warnf("Invalid path in %s: %q", sslCertDir, err)
		}
	}
	if sslCertFileName != "" || sslCertDirName != "" {
		// If we copied certs via env then also make the to set the env in the VM.
		files = append(files, getSSLEnvironmentFiles(sslCertFileName, sslCertDirName)...)
	}

	files = append(files, File{
		Node: Node{
			User:  GetNodeUsr("root"),
			Group: GetNodeGrp("root"),
			Path:  "/etc/chrony.conf",
		},
		FileEmbedded1: FileEmbedded1{
			Append: []Resource{{
				Source: EncodeDataURLPtr("\nconfdir /etc/chrony.d\n"),
			}},
		},
	})

	// Issue #11541: allow Chrony to update the system time when it has drifted
	// far from NTP time.
	files = append(files, File{
		Node: Node{
			User:  GetNodeUsr("root"),
			Group: GetNodeGrp("root"),
			Path:  "/etc/chrony.d/50-podman-makestep.conf",
		},
		FileEmbedded1: FileEmbedded1{
			Contents: Resource{
				Source: EncodeDataURLPtr("makestep 1 -1\n"),
			},
		},
	})

	// Only necessary for qemu on mac
	if netRecover {
		files = append(files, File{
			Node: Node{
				User:  GetNodeUsr("root"),
				Group: GetNodeGrp("root"),
				Path:  "/usr/local/bin/net-health-recovery.sh",
			},
			FileEmbedded1: FileEmbedded1{
				Mode: IntToPtr(0755),
				Contents: Resource{
					Source: EncodeDataURLPtr(GetNetRecoveryFile()),
				},
			},
		})
	}

	return files
}

func getCerts(certsDir string, isDir bool) []File {
	var (
		files []File
	)

	if isDir {
		err := filepath.WalkDir(certsDir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				certPath, err := filepath.Rel(certsDir, path)
				if err != nil {
					logrus.Warnf("%s", err)
					return nil
				}

				file, err := prepareCertFile(filepath.Join(certsDir, certPath), certPath)
				if err == nil {
					files = append(files, file)
				}
			}

			return nil
		})
		if err != nil {
			if !os.IsNotExist(err) {
				logrus.Warnf("Unable to copy certs via ignition, error while reading certs from %s:  %s", certsDir, err.Error())
			}
		}
	} else {
		fileName := filepath.Base(certsDir)
		file, err := prepareCertFile(certsDir, fileName)
		if err == nil {
			files = append(files, file)
		}
	}

	return files
}

func prepareCertFile(fpath string, name string) (File, error) {
	b, err := os.ReadFile(fpath)
	if err != nil {
		logrus.Warnf("Unable to read cert file %v", err)
		return File{}, err
	}

	// Note path is required here as we always create a path for the linux VM
	// even when the client run on windows so we cannot use filepath.
	targetPath := path.Join(define.UserCertsTargetPath, name)

	logrus.Debugf("Copying cert file from '%s' to '%s'.", fpath, targetPath)

	file := File{
		Node: Node{
			Group: GetNodeGrp("root"),
			Path:  targetPath,
			User:  GetNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: EncodeDataURLPtr(string(b)),
			},
			Mode: IntToPtr(0644),
		},
	}
	return file, nil
}

const (
	systemdSSLConf = "/etc/systemd/system.conf.d/podman-machine-ssl.conf"
	envdSSLConf    = "/etc/environment.d/podman-machine-ssl.conf"
	profileSSLConf = "/etc/profile.d/podman-machine-ssl.sh"
	sslCertFile    = "SSL_CERT_FILE"
	sslCertDir     = "SSL_CERT_DIR"
)

func getSSLEnvironmentFiles(sslFileName, sslDirName string) []File {
	systemdFileContent := "[Manager]\n"
	envdFileContent := ""
	profileFileContent := ""
	if sslFileName != "" {
		// certs are written to UserCertsTargetPath see prepareCertFile()
		// Note the mix of path/filepath is intentional and required, we want to get the name of
		// a path on the client (i.e. windows) but then join to linux path that will be used inside the VM.
		env := fmt.Sprintf("%s=%q\n", sslCertFile, path.Join(define.UserCertsTargetPath, filepath.Base(sslFileName)))
		systemdFileContent += "DefaultEnvironment=" + env
		envdFileContent += env
		profileFileContent += "export " + env
	}
	if sslDirName != "" {
		// certs are written to UserCertsTargetPath see prepareCertFile()
		env := fmt.Sprintf("%s=%q\n", sslCertDir, define.UserCertsTargetPath)
		systemdFileContent += "DefaultEnvironment=" + env
		envdFileContent += env
		profileFileContent += "export " + env
	}
	return []File{
		getSSLFile(systemdSSLConf, systemdFileContent),
		getSSLFile(envdSSLConf, envdFileContent),
		getSSLFile(profileSSLConf, profileFileContent),
	}
}

func getSSLFile(path, content string) File {
	return File{
		Node: Node{
			Group: GetNodeGrp("root"),
			Path:  path,
			User:  GetNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Contents: Resource{
				Source: EncodeDataURLPtr(content),
			},
			Mode: IntToPtr(0644),
		},
	}
}

func getLinks(usrName string) []Link {
	return []Link{{
		Node: Node{
			Group: GetNodeGrp(usrName),
			Path:  "/home/" + usrName + "/.config/systemd/user/default.target.wants/linger-example.service",
			User:  GetNodeUsr(usrName),
		},
		LinkEmbedded1: LinkEmbedded1{
			Hard:   BoolToPtr(false),
			Target: "/home/" + usrName + "/.config/systemd/user/linger-example.service",
		},
	}, {
		Node: Node{
			Group:     GetNodeGrp("root"),
			Path:      "/usr/local/bin/docker",
			Overwrite: BoolToPtr(true),
			User:      GetNodeUsr("root"),
		},
		LinkEmbedded1: LinkEmbedded1{
			Hard:   BoolToPtr(false),
			Target: "/usr/bin/podman",
		},
	}}
}

func EncodeDataURLPtr(contents string) *string {
	return StrToPtr(fmt.Sprintf("data:,%s", url.PathEscape(contents)))
}

func GetPodmanDockerTmpConfig(uid int, rootful bool, newline bool) string {
	// Derived from https://github.com/containers/podman/blob/main/contrib/systemd/system/podman-docker.conf
	podmanSock := "/run/podman/podman.sock"
	if !rootful {
		podmanSock = fmt.Sprintf("/run/user/%d/podman/podman.sock", uid)
	}
	suffix := ""
	if newline {
		suffix = "\n"
	}

	return fmt.Sprintf("L+  /run/docker.sock   -    -    -     -   %s%s", podmanSock, suffix)
}

// SetIgnitionFile creates a new Machine File for the machine's ignition file
// and assigns the handle to `loc`
func SetIgnitionFile(loc *define.VMFile, vmtype define.VMType, vmName, vmConfigDir string) error {
	ignitionFile, err := define.NewMachineFile(filepath.Join(vmConfigDir, vmName+".ign"), nil)
	if err != nil {
		return err
	}

	*loc = *ignitionFile
	return nil
}

type IgnitionBuilder struct {
	dynamicIgnition DynamicIgnition
	units           []Unit
}

// NewIgnitionBuilder generates a new IgnitionBuilder type using the
// base `DynamicIgnition` object
func NewIgnitionBuilder(dynamicIgnition DynamicIgnition) IgnitionBuilder {
	return IgnitionBuilder{
		dynamicIgnition,
		[]Unit{},
	}
}

// GenerateIgnitionConfig generates the ignition config
func (i *IgnitionBuilder) GenerateIgnitionConfig() error {
	return i.dynamicIgnition.GenerateIgnitionConfig()
}

// WithUnit adds systemd units to the internal `DynamicIgnition` config
func (i *IgnitionBuilder) WithUnit(units ...Unit) {
	i.dynamicIgnition.Cfg.Systemd.Units = append(i.dynamicIgnition.Cfg.Systemd.Units, units...)
}

// WithFile adds storage files to the internal `DynamicIgnition` config
func (i *IgnitionBuilder) WithFile(files ...File) {
	i.dynamicIgnition.Cfg.Storage.Files = append(i.dynamicIgnition.Cfg.Storage.Files, files...)
}

// BuildWithIgnitionFile copies the provided ignition file into the internal
// `DynamicIgnition` write path
func (i *IgnitionBuilder) BuildWithIgnitionFile(ignPath string) error {
	inputIgnition, err := os.ReadFile(ignPath)
	if err != nil {
		return err
	}

	return os.WriteFile(i.dynamicIgnition.WritePath, inputIgnition, 0644)
}

// Build writes the internal `DynamicIgnition` config to its write path
func (i *IgnitionBuilder) Build() error {
	logrus.Debugf("writing ignition file to %q", i.dynamicIgnition.WritePath)
	return i.dynamicIgnition.Write()
}

func GetNetRecoveryFile() string {
	return `#!/bin/bash
# Verify network health, and bounce the network device if host connectivity
# is lost. This is a temporary workaround for a known rare qemu/virtio issue
# that affects some systems

sleep 120 # allow time for network setup on initial boot
while true; do
  sleep 30
  curl -s -o /dev/null --max-time 30 http://192.168.127.1/health
  if [ "$?" != "0" ]; then
    echo "bouncing nic due to loss of connectivity with host"
    ifconfig enp0s1 down; ifconfig enp0s1 up
  fi
done
`
}

func GetNetRecoveryUnitFile() *parser.UnitFile {
	recoveryUnit := parser.NewUnitFile()
	recoveryUnit.Add("Unit", "Description", "Verifies health of network and recovers if necessary")
	recoveryUnit.Add("Unit", "After", "sshd.socket sshd.service")
	recoveryUnit.Add("Service", "ExecStart", "/usr/local/bin/net-health-recovery.sh")
	recoveryUnit.Add("Service", "StandardOutput", "journal")
	recoveryUnit.Add("Service", "StandardError", "journal")
	recoveryUnit.Add("Service", "StandardInput", "null")
	recoveryUnit.Add("Install", "WantedBy", "default.target")

	return recoveryUnit
}

func DefaultReadyUnitFile() parser.UnitFile {
	u := parser.NewUnitFile()
	u.Add("Unit", "After", "sshd.socket sshd.service")
	u.Add("Unit", "OnFailure", "emergency.target")
	u.Add("Unit", "OnFailureJobMode", "isolate")
	u.Add("Service", "Type", "oneshot")
	u.Add("Service", "RemainAfterExit", "yes")
	u.Add("Install", "RequiredBy", "default.target")
	return *u
}
