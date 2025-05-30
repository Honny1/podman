//go:build !remote

package infra

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/containers/common/pkg/cgroups"
	"github.com/containers/podman/v5/libpod"
	"github.com/containers/podman/v5/pkg/domain/entities"
	"github.com/containers/podman/v5/pkg/namespaces"
	"github.com/containers/podman/v5/pkg/rootless"
	"github.com/containers/podman/v5/pkg/util"
	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/types"
	"github.com/sirupsen/logrus"
	flag "github.com/spf13/pflag"
)

var (
	// runtimeSync only guards the non-specialized runtime
	runtimeSync sync.Once
	// The default GetRuntime() always returns the same object and error
	runtimeLib *libpod.Runtime
	runtimeErr error
)

type engineOpts struct {
	withFDS  bool
	reset    bool
	renumber bool
	config   *entities.PodmanConfig
}

// GetRuntime generates a new libpod runtime configured by command line options
func GetRuntime(ctx context.Context, flags *flag.FlagSet, cfg *entities.PodmanConfig) (*libpod.Runtime, error) {
	runtimeSync.Do(func() {
		runtimeLib, runtimeErr = getRuntime(ctx, flags, &engineOpts{
			withFDS:  true,
			reset:    cfg.IsReset,
			renumber: cfg.IsRenumber,
			config:   cfg,
		})
	})
	return runtimeLib, runtimeErr
}

func getRuntime(ctx context.Context, fs *flag.FlagSet, opts *engineOpts) (*libpod.Runtime, error) {
	options := []libpod.RuntimeOption{}
	storageOpts := types.StoreOptions{}
	cfg := opts.config

	storageSet := false

	uidmapFlag := fs.Lookup("uidmap")
	gidmapFlag := fs.Lookup("gidmap")
	subuidname := fs.Lookup("subuidname")
	subgidname := fs.Lookup("subgidname")
	if (uidmapFlag != nil && gidmapFlag != nil && subuidname != nil && subgidname != nil) &&
		(uidmapFlag.Changed || gidmapFlag.Changed || subuidname.Changed || subgidname.Changed) {
		userns, _ := fs.GetString("userns")
		uidmapVal, _ := fs.GetStringSlice("uidmap")
		gidmapVal, _ := fs.GetStringSlice("gidmap")
		subuidVal, _ := fs.GetString("subuidname")
		subgidVal, _ := fs.GetString("subgidname")
		mappings, err := ParseIDMapping(namespaces.UsernsMode(userns), uidmapVal, gidmapVal, subuidVal, subgidVal)
		if err != nil {
			return nil, err
		}
		storageOpts.UIDMap = mappings.UIDMap
		storageOpts.GIDMap = mappings.GIDMap

		storageSet = true
	}

	if fs.Changed("root") {
		storageSet = true
		storageOpts.GraphRoot = cfg.GraphRoot
		storageOpts.GraphDriverOptions = []string{}
	}
	if fs.Changed("runroot") {
		storageSet = true
		storageOpts.RunRoot = cfg.Runroot
	}
	if fs.Changed("imagestore") {
		storageSet = true
		storageOpts.ImageStore = cfg.ImageStore
		options = append(options, libpod.WithImageStore(cfg.ImageStore))
	}
	if fs.Changed("pull-option") {
		storageSet = true
		storageOpts.PullOptions = make(map[string]string)
		for _, v := range cfg.PullOptions {
			if v == "" {
				continue
			}
			val := strings.SplitN(v, "=", 2)
			if len(val) != 2 {
				return nil, fmt.Errorf("invalid pull option: %s", v)
			}
			storageOpts.PullOptions[val[0]] = val[1]
		}
	}
	if fs.Changed("storage-driver") {
		storageSet = true
		storageOpts.GraphDriverName = cfg.StorageDriver
		// Overriding the default storage driver caused GraphDriverOptions from storage.conf to be ignored
		storageOpts.GraphDriverOptions = []string{}
	}
	// This should always be checked after storage-driver is checked
	if len(cfg.StorageOpts) > 0 {
		storageSet = true
		if len(cfg.StorageOpts) == 1 && cfg.StorageOpts[0] == "" {
			storageOpts.GraphDriverOptions = []string{}
		} else {
			storageOpts.GraphDriverOptions = cfg.StorageOpts
		}
	}
	if fs.Changed("transient-store") {
		options = append(options, libpod.WithTransientStore(cfg.TransientStore))
	}

	if opts.reset {
		options = append(options, libpod.WithReset())
	}
	if opts.renumber {
		options = append(options, libpod.WithRenumber())
	}

	if len(cfg.RuntimeFlags) > 0 {
		runtimeFlags := []string{}
		for _, arg := range cfg.RuntimeFlags {
			runtimeFlags = append(runtimeFlags, "--"+arg)
		}
		options = append(options, libpod.WithRuntimeFlags(runtimeFlags))
	}

	// Only set this if the user changes storage config on the command line
	if storageSet {
		options = append(options, libpod.WithStorageConfig(storageOpts))
	}

	// TODO CLI flags for image config?
	// TODO CLI flag for signature policy?

	if len(cfg.ContainersConf.Engine.Namespace) > 0 {
		options = append(options, libpod.WithNamespace(cfg.ContainersConf.Engine.Namespace))
	}

	if fs.Changed("runtime") {
		options = append(options, libpod.WithOCIRuntime(cfg.RuntimePath))
	}

	if fs.Changed("conmon") {
		options = append(options, libpod.WithConmonPath(cfg.ConmonPath))
	}
	if fs.Changed("tmpdir") {
		options = append(options, libpod.WithTmpDir(cfg.ContainersConf.Engine.TmpDir))
	}
	if fs.Changed("network-cmd-path") {
		options = append(options, libpod.WithNetworkCmdPath(cfg.ContainersConf.Engine.NetworkCmdPath))
	}
	if fs.Changed("network-backend") {
		options = append(options, libpod.WithNetworkBackend(cfg.ContainersConf.Network.NetworkBackend))
	}

	if fs.Changed("events-backend") {
		options = append(options, libpod.WithEventsLogger(cfg.ContainersConf.Engine.EventsLogger))
	}

	if fs.Changed("volumepath") {
		options = append(options, libpod.WithVolumePath(cfg.ContainersConf.Engine.VolumePath))
	}

	if fs.Changed("cgroup-manager") {
		options = append(options, libpod.WithCgroupManager(cfg.ContainersConf.Engine.CgroupManager))
	} else {
		unified, err := cgroups.IsCgroup2UnifiedMode()
		if err != nil {
			return nil, err
		}
		if rootless.IsRootless() && !unified {
			options = append(options, libpod.WithCgroupManager("cgroupfs"))
		}
	}

	// TODO flag to set libpod static dir?
	// TODO flag to set libpod tmp dir?

	if fs.Changed("network-config-dir") {
		options = append(options, libpod.WithNetworkConfigDir(cfg.ContainersConf.Network.NetworkConfigDir))
	}
	if fs.Changed("default-mounts-file") {
		options = append(options, libpod.WithDefaultMountsFile(cfg.ContainersConf.Containers.DefaultMountsFile))
	}
	if fs.Changed("hooks-dir") {
		options = append(options, libpod.WithHooksDir(cfg.ContainersConf.Engine.HooksDir.Get()...))
	}
	if fs.Changed("registries-conf") {
		options = append(options, libpod.WithRegistriesConf(cfg.RegistriesConf))
	}

	if fs.Changed("db-backend") {
		options = append(options, libpod.WithDatabaseBackend(cfg.ContainersConf.Engine.DBBackend))
	}

	if cfg.CdiSpecDirs != nil {
		options = append(options, libpod.WithCDISpecDirs(cfg.CdiSpecDirs))
	}

	if cfg.Syslog {
		options = append(options, libpod.WithSyslog())
	}

	if opts.config.ContainersConfDefaultsRO.Engine.StaticDir != "" {
		options = append(options, libpod.WithStaticDir(opts.config.ContainersConfDefaultsRO.Engine.StaticDir))
	}

	// TODO flag to set CNI plugins dir?

	if !opts.withFDS {
		options = append(options, libpod.WithEnableSDNotify())
	}
	return libpod.NewRuntime(ctx, options...)
}

// ParseIDMapping takes idmappings and subuid and subgid maps and returns a storage mapping
func ParseIDMapping(mode namespaces.UsernsMode, uidMapSlice, gidMapSlice []string, subUIDMap, subGIDMap string) (*types.IDMappingOptions, error) {
	options := types.IDMappingOptions{
		HostUIDMapping: true,
		HostGIDMapping: true,
	}

	if mode.IsAuto() {
		var err error
		options.HostUIDMapping = false
		options.HostGIDMapping = false
		options.AutoUserNs = true
		opts, err := util.GetAutoOptions(mode)
		if err != nil {
			return nil, err
		}
		options.AutoUserNsOpts = *opts
		return &options, nil
	}

	if subGIDMap == "" && subUIDMap != "" {
		subGIDMap = subUIDMap
	}
	if subUIDMap == "" && subGIDMap != "" {
		subUIDMap = subGIDMap
	}
	if len(gidMapSlice) == 0 && len(uidMapSlice) != 0 {
		gidMapSlice = uidMapSlice
	}
	if len(uidMapSlice) == 0 && len(gidMapSlice) != 0 {
		uidMapSlice = gidMapSlice
	}
	if len(uidMapSlice) == 0 && subUIDMap == "" && os.Getuid() != 0 {
		uidMapSlice = []string{fmt.Sprintf("0:%d:1", os.Getuid())}
	}
	if len(gidMapSlice) == 0 && subGIDMap == "" && os.Getuid() != 0 {
		gidMapSlice = []string{fmt.Sprintf("0:%d:1", os.Getgid())}
	}

	if subUIDMap != "" && subGIDMap != "" {
		mappings, err := idtools.NewIDMappings(subUIDMap, subGIDMap)
		if err != nil {
			return nil, err
		}
		options.UIDMap = mappings.UIDs()
		options.GIDMap = mappings.GIDs()
	}

	parentUIDMap, parentGIDMap, err := rootless.GetAvailableIDMaps()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The kernel-provided files only exist if user namespaces are supported
			logrus.Debugf("User or group ID mappings not available: %s", err)
		} else {
			return nil, err
		}
	}

	parsedUIDMap, err := util.ParseIDMap(uidMapSlice, "UID", parentUIDMap)
	if err != nil {
		return nil, err
	}
	parsedGIDMap, err := util.ParseIDMap(gidMapSlice, "GID", parentGIDMap)
	if err != nil {
		return nil, err
	}
	options.UIDMap = append(options.UIDMap, parsedUIDMap...)
	options.GIDMap = append(options.GIDMap, parsedGIDMap...)
	if len(options.UIDMap) > 0 {
		options.HostUIDMapping = false
	}
	if len(options.GIDMap) > 0 {
		options.HostGIDMapping = false
	}
	return &options, nil
}

// StartWatcher starts a new SIGHUP go routine for the current config.
func StartWatcher(rt *libpod.Runtime) {
	// Set up the signal notifier
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)

	go func() {
		for {
			// Block until the signal is received
			logrus.Debugf("waiting for SIGHUP to reload configuration")
			<-ch
			if err := rt.Reload(); err != nil {
				logrus.Errorf("Unable to reload configuration: %v", err)
				continue
			}
		}
	}()

	logrus.Debugf("registered SIGHUP watcher for config")
}
