// This file is part of arduino-cli.
//
// Copyright 2020 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU General Public License version 3,
// which covers the main part of arduino-cli.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/gpl-3.0.en.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

package packagemanager

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/arduino/arduino-cli/arduino"
	"github.com/arduino/arduino-cli/arduino/cores"
	"github.com/arduino/arduino-cli/arduino/discovery"
	"github.com/arduino/arduino-cli/configuration"
	"github.com/arduino/go-paths-helper"
	properties "github.com/arduino/go-properties-orderedmap"
	"github.com/pkg/errors"
	semver "go.bug.st/relaxed-semver"
)

// LoadHardware read all plaforms from the configured paths
func (pm *Builder) LoadHardware() []error {
	hardwareDirs := configuration.HardwareDirectories(configuration.Settings)
	merr := pm.LoadHardwareFromDirectories(hardwareDirs)

	bundleToolDirs := configuration.BuiltinToolsDirectories(configuration.Settings)
	merr = append(merr, pm.LoadToolsFromBundleDirectories(bundleToolDirs)...)

	return merr
}

// LoadHardwareFromDirectories load plaforms from a set of directories
func (pm *Builder) LoadHardwareFromDirectories(hardwarePaths paths.PathList) []error {
	var merr []error
	for _, path := range hardwarePaths {
		merr = append(merr, pm.LoadHardwareFromDirectory(path)...)
	}
	return merr
}

// LoadHardwareFromDirectory read a plaform from the path passed as parameter
func (pm *Builder) LoadHardwareFromDirectory(path *paths.Path) []error {
	var merr []error
	pm.log.Infof("Loading hardware from: %s", path)
	if err := path.ToAbs(); err != nil {
		return append(merr, fmt.Errorf("%s: %w", tr("finding absolute path of %s", path), err))
	}

	if path.IsNotDir() {
		return append(merr, errors.New(tr("%s is not a directory", path)))
	}

	// Scan subdirs
	packagersPaths, err := path.ReadDir()
	if err != nil {
		return append(merr, fmt.Errorf("%s: %w", tr("reading directory %s", path), err))
	}
	packagersPaths.FilterOutHiddenFiles()
	packagersPaths.FilterDirs()

	// Load custom platform properties if available
	// "Global" platform.txt used to overwrite all installed platforms.
	// For more info: https://arduino.github.io/arduino-cli/latest/platform-specification/#global-platformtxt
	if globalPlatformTxt := path.Join("platform.txt"); globalPlatformTxt.Exist() {
		pm.log.Infof("Loading custom platform properties: %s", globalPlatformTxt)
		if p, err := properties.LoadFromPath(globalPlatformTxt); err != nil {
			pm.log.WithError(err).Errorf("Error loading properties.")
		} else {
			pm.packagesCustomGlobalProperties.Merge(p)
		}
	}

	for _, packagerPath := range packagersPaths {
		packager := packagerPath.Base()

		// Skip tools, they're not packages and don't contain Platforms
		if packager == "tools" {
			pm.log.Infof("Excluding directory: %s", packagerPath)
			continue
		}

		// Follow symlinks
		err := packagerPath.FollowSymLink() // ex: .arduino15/packages/arduino/
		if err != nil {
			merr = append(merr, fmt.Errorf("%s: %w", tr("following symlink %s", path), err))
			continue
		}

		// There are two possible package directory structures:
		// - PACKAGER/ARCHITECTURE-1/boards.txt...                   (ex: arduino/avr/...)
		//   PACKAGER/ARCHITECTURE-2/boards.txt...                   (ex: arduino/sam/...)
		//   PACKAGER/ARCHITECTURE-3/boards.txt...                   (ex: arduino/samd/...)
		// or
		// - PACKAGER/hardware/ARCHITECTURE-1/VERSION/boards.txt...  (ex: arduino/hardware/avr/1.6.15/...)
		//   PACKAGER/hardware/ARCHITECTURE-2/VERSION/boards.txt...  (ex: arduino/hardware/sam/1.6.6/...)
		//   PACKAGER/hardware/ARCHITECTURE-3/VERSION/boards.txt...  (ex: arduino/hardware/samd/1.6.12/...)
		//   PACKAGER/tools/...                                      (ex: arduino/tools/...)
		// in the latter case we just move into "hardware" directory and continue
		var architectureParentPath *paths.Path
		hardwareSubdirPath := packagerPath.Join("hardware") // ex: .arduino15/packages/arduino/hardware
		if hardwareSubdirPath.IsDir() {
			// we found the "hardware" directory move down into that
			architectureParentPath = hardwareSubdirPath // ex: .arduino15/packages/arduino/
		} else {
			// we are already at the correct level
			architectureParentPath = packagerPath
		}

		targetPackage := pm.packages.GetOrCreatePackage(packager)
		merr = append(merr, pm.loadPlatforms(targetPackage, architectureParentPath)...)

		// Check if we have tools to load, the directory structure is as follows:
		// - PACKAGER/tools/TOOL-NAME/TOOL-VERSION/... (ex: arduino/tools/bossac/1.7.0/...)
		toolsSubdirPath := packagerPath.Join("tools")
		if toolsSubdirPath.IsDir() {
			pm.log.Infof("Checking existence of 'tools' path: %s", toolsSubdirPath)
			merr = append(merr, pm.LoadToolsFromPackageDir(targetPackage, toolsSubdirPath)...)
		}
		// If the Package does not contain Platforms or Tools we remove it since does not contain anything valuable
		if len(targetPackage.Platforms) == 0 && len(targetPackage.Tools) == 0 {
			delete(pm.packages, packager)
		}
	}

	return merr
}

// loadPlatforms load plaftorms from the specified directory assuming that they belongs
// to the targetPackage object passed as parameter.
// A list of gRPC Status error is returned for each Platform failed to load.
func (pm *Builder) loadPlatforms(targetPackage *cores.Package, packageDir *paths.Path) []error {
	pm.log.Infof("Loading package %s from: %s", targetPackage.Name, packageDir)

	var merr []error

	platformsDirs, err := packageDir.ReadDir()
	if err != nil {
		return append(merr, fmt.Errorf("%s: %w", tr("reading directory %s", packageDir), err))
	}

	// A platform can only be inside a directory, thus we skip everything else.
	platformsDirs.FilterDirs()
	// Filter out directories like .git and similar things
	platformsDirs.FilterOutPrefix(".")
	for _, platformPath := range platformsDirs {
		targetArchitecture := platformPath.Base()

		// Tools are not a platform
		if targetArchitecture == "tools" {
			continue
		}
		if err := pm.loadPlatform(targetPackage, targetArchitecture, platformPath); err != nil {
			merr = append(merr, err)
		}
	}

	return merr
}

// loadPlatform loads a single platform and all its installed releases given a platformPath.
// platformPath must be a directory.
// Returns a gRPC Status error in case of failures.
func (pm *Builder) loadPlatform(targetPackage *cores.Package, architecture string, platformPath *paths.Path) error {
	// This is not a platform
	if platformPath.IsNotDir() {
		return errors.New(tr("path is not a platform directory: %s", platformPath))
	}

	// There are two possible platform directory structures:
	// - ARCHITECTURE/boards.txt
	// - ARCHITECTURE/VERSION/boards.txt
	// We identify them by checking where is the bords.txt file
	possibleBoardTxtPath := platformPath.Join("boards.txt")
	if exist, err := possibleBoardTxtPath.ExistCheck(); err != nil {
		return fmt.Errorf("%s: %w", tr("looking for boards.txt in %s", possibleBoardTxtPath), err)
	} else if exist {
		// case: ARCHITECTURE/boards.txt

		platformTxtPath := platformPath.Join("platform.txt")
		platformProperties, err := properties.SafeLoad(platformTxtPath.String())
		if err != nil {
			return fmt.Errorf("%s: %w", tr("loading platform.txt"), err)
		}

		versionString := platformProperties.ExpandPropsInString(platformProperties.Get("version"))
		version, err := semver.Parse(versionString)
		if err != nil {
			return &arduino.InvalidVersionError{Cause: fmt.Errorf("%s: %s", platformTxtPath, err)}
		}

		// Check if package_bundled_index.json exists.
		// This is used indirectly by the Java IDE since it's necessary for the arduino-builder
		// to find cores bundled with that version of the IDE.
		// TODO: This piece of logic MUST be removed as soon as the Java IDE stops using the arduino-builder.
		isIDEBundled := false
		packageBundledIndexPath := platformPath.Parent().Parent().Join("package_index_bundled.json")
		if packageBundledIndexPath.Exist() {
			// particular case: ARCHITECTURE/boards.txt with package_bundled_index.json

			// this is an unversioned Platform with a package_index_bundled.json that
			// gives information about the version and tools needed

			// Parse the bundled index and merge to the general index
			index, err := pm.LoadPackageIndexFromFile(packageBundledIndexPath)
			if err != nil {
				return fmt.Errorf("%s: %w", tr("parsing IDE bundled index"), err)
			}

			// Now export the bundled index in a temporary core.Packages to retrieve the bundled package version
			tmp := cores.NewPackages()
			index.MergeIntoPackages(tmp)
			if tmpPackage := tmp.GetOrCreatePackage(targetPackage.Name); tmpPackage == nil {
				pm.log.Warnf("Can't determine bundle platform version for %s", targetPackage.Name)
			} else if tmpPlatform := tmpPackage.GetOrCreatePlatform(architecture); tmpPlatform == nil {
				pm.log.Warnf("Can't determine bundle platform version for %s:%s", targetPackage.Name, architecture)
			} else if tmpPlatformRelease := tmpPlatform.GetLatestRelease(); tmpPlatformRelease == nil {
				pm.log.Warnf("Can't determine bundle platform version for %s:%s, no valid release found", targetPackage.Name, architecture)
			} else {
				version = tmpPlatformRelease.Version
			}

			isIDEBundled = true
		}

		platform := targetPackage.GetOrCreatePlatform(architecture)
		if !isIDEBundled {
			platform.ManuallyInstalled = true
		}
		release := platform.GetOrCreateRelease(version)
		release.IsIDEBundled = isIDEBundled
		if isIDEBundled {
			pm.log.Infof("Package is built-in")
		}
		if err := pm.loadPlatformRelease(release, platformPath); err != nil {
			return fmt.Errorf("%s: %w", tr("loading platform release %s", release), err)
		}
		pm.log.WithField("platform", release).Infof("Loaded platform")

	} else {
		// case: ARCHITECTURE/VERSION/boards.txt
		// let's dive into VERSION directories

		versionDirs, err := platformPath.ReadDir()
		if err != nil {
			return fmt.Errorf("%s: %w", tr("reading directory %s", platformPath), err)
		}
		versionDirs.FilterDirs()
		versionDirs.FilterOutHiddenFiles()
		for _, versionDir := range versionDirs {
			if exist, err := versionDir.Join("boards.txt").ExistCheck(); err != nil {
				return fmt.Errorf("%s: %w", tr("opening boards.txt"), err)
			} else if !exist {
				continue
			}

			version, err := semver.Parse(versionDir.Base())
			if err != nil {
				return fmt.Errorf("%s: %w", tr("invalid version directory %s", versionDir), err)
			}
			platform := targetPackage.GetOrCreatePlatform(architecture)
			release := platform.GetOrCreateRelease(version)
			if err := pm.loadPlatformRelease(release, versionDir); err != nil {
				return fmt.Errorf("%s: %w", tr("loading platform release %s", release), err)
			}
			pm.log.WithField("platform", release).Infof("Loaded platform")
		}
	}

	return nil
}

func (pm *Builder) loadPlatformRelease(platform *cores.PlatformRelease, path *paths.Path) error {
	platform.InstallDir = path

	// Some useful paths
	installedJSONPath := path.Join("installed.json")
	platformTxtPath := path.Join("platform.txt")
	platformTxtLocalPath := path.Join("platform.local.txt")
	programmersTxtPath := path.Join("programmers.txt")

	// If the installed.json file is found load it, this is done to handle the
	// case in which the platform's index and its url have been deleted locally,
	// if we don't load it some information about the platform is lost
	if installedJSONPath.Exist() {
		if _, err := pm.LoadPackageIndexFromFile(installedJSONPath); err != nil {
			return fmt.Errorf(tr("loading %[1]s: %[2]s"), installedJSONPath, err)
		}
	}

	// Create platform properties
	platform.Properties = platform.Properties.Clone() // TODO: why CLONE?
	if p, err := properties.SafeLoad(platformTxtPath.String()); err == nil {
		platform.Properties.Merge(p)
	} else {
		return fmt.Errorf(tr("loading %[1]s: %[2]s"), platformTxtPath, err)
	}
	if p, err := properties.SafeLoad(platformTxtLocalPath.String()); err == nil {
		platform.Properties.Merge(p)
	} else {
		return fmt.Errorf(tr("loading %[1]s: %[2]s"), platformTxtLocalPath, err)
	}

	if platform.Properties.SubTree("pluggable_discovery").Size() > 0 {
		platform.PluggableDiscoveryAware = true
	} else {
		platform.Properties.Set("pluggable_discovery.required.0", "builtin:serial-discovery")
		platform.Properties.Set("pluggable_discovery.required.1", "builtin:mdns-discovery")
		platform.Properties.Set("pluggable_monitor.required.serial", "builtin:serial-monitor")
	}

	if platform.Platform.Name == "" {
		if name, ok := platform.Properties.GetOk("name"); ok {
			platform.Platform.Name = name
		} else {
			// If the platform.txt file doesn't exist for this platform and it's not in any
			// package index there is no way of retrieving its name, so we build one using
			// the available information, that is the packager name and the architecture.
			platform.Platform.Name = fmt.Sprintf("%s-%s", platform.Platform.Package.Name, platform.Platform.Architecture)
		}
	}

	// Create programmers properties
	if programmersProperties, err := properties.SafeLoad(programmersTxtPath.String()); err == nil {
		for programmerID, programmerProps := range programmersProperties.FirstLevelOf() {
			if !platform.PluggableDiscoveryAware {
				convertUploadToolsToPluggableDiscovery(programmerProps)
			}
			platform.Programmers[programmerID] = pm.loadProgrammer(programmerProps)
			platform.Programmers[programmerID].PlatformRelease = platform
		}
	} else {
		return err
	}

	if err := pm.loadBoards(platform); err != nil {
		return fmt.Errorf(tr("loading boards: %s"), err)
	}

	if !platform.PluggableDiscoveryAware {
		convertLegacyPlatformToPluggableDiscovery(platform)
	}

	// Build pluggable monitor references
	platform.Monitors = map[string]*cores.MonitorDependency{}
	for protocol, ref := range platform.Properties.SubTree("pluggable_monitor.required").AsMap() {
		split := strings.Split(ref, ":")
		if len(split) != 2 {
			return fmt.Errorf(tr("invalid pluggable monitor reference: %s"), ref)
		}
		pm.log.WithField("protocol", protocol).WithField("tool", ref).Info("Adding monitor tool")
		platform.Monitors[protocol] = &cores.MonitorDependency{
			Packager: split[0],
			Name:     split[1],
		}
	}

	// Support for pluggable monitors in debugging/development environments
	platform.MonitorsDevRecipes = map[string]string{}
	for protocol, recipe := range platform.Properties.SubTree("pluggable_monitor.pattern").AsMap() {
		pm.log.WithField("protocol", protocol).WithField("recipe", recipe).Info("Adding monitor recipe")
		platform.MonitorsDevRecipes[protocol] = recipe
	}

	return nil
}

func convertLegacyPlatformToPluggableDiscovery(platform *cores.PlatformRelease) {
	toolsProps := platform.Properties.SubTree("tools").FirstLevelOf()
	for toolName, toolProps := range toolsProps {
		if !toolProps.ContainsKey("upload.network_pattern") {
			continue
		}

		// Convert network_pattern configuration to pluggable discovery
		convertedToolName := toolName + "__pluggable_network"
		convertedProps := convertLegacyNetworkPatternToPluggableDiscovery(toolProps, convertedToolName)

		// Merge the converted properties in the root configuration
		platform.Properties.Merge(convertedProps)

		// Add the network upload to the boards using the old method
		for _, board := range platform.Boards {
			oldUploadTool := board.Properties.Get("upload.tool")
			if oldUploadTool == toolName && !board.Properties.ContainsKey("upload.tool.network") {
				board.Properties.Set("upload.tool.network", convertedToolName)

				// Add identification properties for network protocol
				i := 0
				for {
					if !board.Properties.ContainsKey(fmt.Sprintf("upload_port.%d.vid", i)) {
						break
					}
					i++
				}
				board.Properties.Set(fmt.Sprintf("upload_port.%d.board", i), board.BoardID)
			}
		}
	}
}

var netPropRegexp = regexp.MustCompile(`\{upload\.network\.([^}]+)\}`)

func convertLegacyNetworkPatternToPluggableDiscovery(props *properties.Map, newToolName string) *properties.Map {
	pattern, ok := props.GetOk("upload.network_pattern")
	if !ok {
		return nil
	}
	props.Remove("upload.network_pattern")
	pattern = strings.ReplaceAll(pattern, "{serial.port}", "{upload.port.address}")
	pattern = strings.ReplaceAll(pattern, "{network.port}", "{upload.port.properties.port}")
	if strings.Contains(pattern, "{network.password}") {
		props.Set("upload.field.password", "Password")
		props.Set("upload.field.password.secret", "true")
		pattern = strings.ReplaceAll(pattern, "{network.password}", "{upload.field.password}")
	}
	// Search for "{upload.network.PROPERTY}"" and convert it to "{upload.port.property.PROPERTY}"
	for netPropRegexp.MatchString(pattern) {
		netProp := netPropRegexp.FindStringSubmatch(pattern)[1]
		pattern = strings.ReplaceAll(pattern, "{upload.network."+netProp+"}", "{upload.port.properties."+netProp+"}")
	}
	props.Set("upload.pattern", pattern)

	prefix := "tools." + newToolName + "."
	res := properties.NewMap()
	for _, k := range props.Keys() {
		v := props.Get(k)
		res.Set(prefix+k, v)
		// fmt.Println("ADDED:", prefix+k+"="+v)
	}
	return res
}

func (pm *Builder) loadProgrammer(programmerProperties *properties.Map) *cores.Programmer {
	return &cores.Programmer{
		Name:       programmerProperties.Get("name"),
		Properties: programmerProperties,
	}
}

func (pm *Builder) loadBoards(platform *cores.PlatformRelease) error {
	if platform.InstallDir == nil {
		return fmt.Errorf(tr("platform not installed"))
	}

	boardsTxtPath := platform.InstallDir.Join("boards.txt")
	boardsProperties, err := properties.LoadFromPath(boardsTxtPath)
	if err != nil {
		return err
	}

	boardsLocalTxtPath := platform.InstallDir.Join("boards.local.txt")
	if localProperties, err := properties.SafeLoadFromPath(boardsLocalTxtPath); err == nil {
		boardsProperties.Merge(localProperties)
	} else {
		return err
	}

	propertiesByBoard := boardsProperties.FirstLevelOf()

	if menus, ok := propertiesByBoard["menu"]; ok {
		platform.Menus = menus
	} else {
		platform.Menus = properties.NewMap()
	}
	// This is not a board id so we remove it to correctly
	// set all other boards properties
	delete(propertiesByBoard, "menu")

	skippedBoards := []string{}
	for boardID, boardProperties := range propertiesByBoard {
		var board *cores.Board
		for key := range boardProperties.AsMap() {
			if !strings.HasPrefix(key, "menu.") {
				continue
			}
			// Menu keys are formed like this:
			//     menu.cache.off=false
			//     menu.cache.on=true
			// so we assume that the a second element in the slice exists
			menuName := strings.Split(key, ".")[1]
			if !platform.Menus.ContainsKey(menuName) {
				fqbn := fmt.Sprintf("%s:%s:%s", platform.Platform.Package.Name, platform.Platform.Architecture, boardID)
				skippedBoards = append(skippedBoards, fqbn)
				goto next_board
			}
		}

		if !platform.PluggableDiscoveryAware {
			convertVidPidIdentificationPropertiesToPluggableDiscovery(boardProperties)
			convertUploadToolsToPluggableDiscovery(boardProperties)
		}

		// The board's ID must be available in a board's properties since it can
		// be used in all configuration files for several reasons, like setting compilation
		// flags depending on the board id.
		// For more info:
		// https://arduino.github.io/arduino-cli/dev/platform-specification/#global-predefined-properties
		boardProperties.Set("_id", boardID)
		board = platform.GetOrCreateBoard(boardID)
		board.Properties.Merge(boardProperties)
	next_board:
	}

	if len(skippedBoards) > 0 {
		return fmt.Errorf(tr("skipping loading of boards %s: malformed custom board options"), strings.Join(skippedBoards, ", "))
	}

	return nil
}

// Converts the old:
//
//   - xxx.vid.N
//   - xxx.pid.N
//
// properties into pluggable discovery compatible:
//
//   - xxx.upload_port.N.vid
//   - xxx.upload_port.N.pid
//
func convertVidPidIdentificationPropertiesToPluggableDiscovery(boardProperties *properties.Map) {
	n := 0
	outputVidPid := func(vid, pid string) {
		boardProperties.Set(fmt.Sprintf("upload_port.%d.vid", n), vid)
		boardProperties.Set(fmt.Sprintf("upload_port.%d.pid", n), pid)
		n++
	}
	if boardProperties.ContainsKey("vid") && boardProperties.ContainsKey("pid") {
		outputVidPid(boardProperties.Get("vid"), boardProperties.Get("pid"))
	}

	for _, k := range boardProperties.Keys() {
		if strings.HasPrefix(k, "vid.") {
			idx, err := strconv.ParseUint(k[4:], 10, 64)
			if err != nil {
				continue
			}
			vidKey := fmt.Sprintf("vid.%d", idx)
			pidKey := fmt.Sprintf("pid.%d", idx)
			vid, vidOk := boardProperties.GetOk(vidKey)
			pid, pidOk := boardProperties.GetOk(pidKey)
			if vidOk && pidOk {
				outputVidPid(vid, pid)
			}
		}
	}
}

func convertUploadToolsToPluggableDiscovery(props *properties.Map) {
	actions := []string{"upload", "bootloader", "program"}
	propsToAdd := properties.NewMap()
	for _, action := range actions {
		action += ".tool"
		defaultAction := action + ".default"
		if !props.ContainsKey(defaultAction) {
			// Search for "menu.MENU-ID.MENU-ITEM.ACTION.tool" (some platforms sets ACTION.tool on
			// submenu config entries). See https://github.com/arduino/arduino-cli/issues/1444
			for key, value := range props.AsMap() {
				if !strings.HasPrefix(key, "menu.") {
					continue
				}
				split := strings.Split(key, ".")
				if len(split) != 5 || split[3]+"."+split[4] != action {
					continue
				}
				prefix := split[0] + "." + split[1] + "." + split[2]
				propsToAdd.Set(prefix+"."+defaultAction, value)
			}
			tool, found := props.GetOk(action)
			if !found {
				// Just skip it, ideally this must never happen but if a platform
				// doesn't define an expected upload.tool, bootloader.tool or program.tool
				// there will be other issues further down the road after this conversion
				continue
			}
			propsToAdd.Set(defaultAction, tool)
		}
	}
	props.Merge(propsToAdd)
}

// LoadToolsFromPackageDir loads a set of tools from the given toolsPath. The tools will be loaded
// in the given *Package.
func (pm *Builder) LoadToolsFromPackageDir(targetPackage *cores.Package, toolsPath *paths.Path) []error {
	pm.log.Infof("Loading tools from dir: %s", toolsPath)

	var merr []error

	toolsPaths, err := toolsPath.ReadDir()
	if err != nil {
		return append(merr, fmt.Errorf("%s: %w", tr("reading directory %s", toolsPath), err))
	}
	toolsPaths.FilterDirs()
	toolsPaths.FilterOutHiddenFiles()
	for _, toolPath := range toolsPaths {
		name := toolPath.Base()
		tool := targetPackage.GetOrCreateTool(name)
		if err = pm.loadToolReleasesFromTool(tool, toolPath); err != nil {
			merr = append(merr, fmt.Errorf("%s: %w", tr("loading tool release in %s", toolPath), err))
		}
	}
	return merr
}

func (pm *Builder) loadToolReleasesFromTool(tool *cores.Tool, toolPath *paths.Path) error {
	toolVersions, err := toolPath.ReadDir()
	if err != nil {
		return err
	}
	toolVersions.FilterDirs()
	toolVersions.FilterOutHiddenFiles()
	for _, versionPath := range toolVersions {
		version := semver.ParseRelaxed(versionPath.Base())
		if err := pm.loadToolReleaseFromDirectory(tool, version, versionPath); err != nil {
			return err
		}
	}

	return nil
}

func (pm *Builder) loadToolReleaseFromDirectory(tool *cores.Tool, version *semver.RelaxedVersion, toolReleasePath *paths.Path) error {
	if absToolReleasePath, err := toolReleasePath.Abs(); err != nil {
		return errors.New(tr("error opening %s", absToolReleasePath))
	} else if !absToolReleasePath.IsDir() {
		return errors.New(tr("%s is not a directory", absToolReleasePath))
	} else {
		toolRelease := tool.GetOrCreateRelease(version)
		toolRelease.InstallDir = absToolReleasePath
		pm.log.WithField("tool", toolRelease).Infof("Loaded tool")
		return nil
	}
}

// LoadToolsFromBundleDirectories FIXMEDOC
func (pm *Builder) LoadToolsFromBundleDirectories(dirs paths.PathList) []error {
	var merr []error
	for _, dir := range dirs {
		if err := pm.LoadToolsFromBundleDirectory(dir); err != nil {
			merr = append(merr, fmt.Errorf("%s: %w", tr("loading bundled tools from %s", dir), err))
		}
	}
	return merr
}

// LoadToolsFromBundleDirectory FIXMEDOC
func (pm *Builder) LoadToolsFromBundleDirectory(toolsPath *paths.Path) error {
	pm.log.Infof("Loading tools from bundle dir: %s", toolsPath)

	// We scan toolsPath content to find a "builtin_tools_versions.txt", if such file exists
	// then the all the tools are available in the same directory, mixed together, and their
	// name and version are written in the "builtin_tools_versions.txt" file.
	// If no "builtin_tools_versions.txt" is found, then the directory structure is the classic
	// TOOLSPATH/TOOL-NAME/TOOL-VERSION and it will be parsed as such and associated to an
	// "unnamed" packager.

	// TODO: get rid of "builtin_tools_versions.txt"

	// Search for builtin_tools_versions.txt
	builtinToolsVersionsTxtPath := ""
	findBuiltInToolsVersionsTxt := func(currentPath string, info os.FileInfo, err error) error {
		if err != nil {
			// Ignore errors
			return nil
		}
		if builtinToolsVersionsTxtPath != "" {
			return filepath.SkipDir
		}
		if info.Name() == "builtin_tools_versions.txt" {
			builtinToolsVersionsTxtPath = currentPath
			return filepath.SkipDir
		}
		return nil
	}
	if err := filepath.Walk(toolsPath.String(), findBuiltInToolsVersionsTxt); err != nil {
		return fmt.Errorf(tr("searching for builtin_tools_versions.txt in %[1]s: %[2]s"), toolsPath, err)
	}

	if builtinToolsVersionsTxtPath != "" {
		// If builtin_tools_versions.txt is found create tools based on the info
		// contained in that file
		pm.log.Infof("Found builtin_tools_versions.txt")
		toolPath, err := paths.New(builtinToolsVersionsTxtPath).Parent().Abs()
		if err != nil {
			return fmt.Errorf(tr("getting parent dir of %[1]s: %[2]s"), builtinToolsVersionsTxtPath, err)
		}

		all, err := properties.Load(builtinToolsVersionsTxtPath)
		if err != nil {
			return fmt.Errorf(tr("reading %[1]s: %[2]s"), builtinToolsVersionsTxtPath, err)
		}

		for packager, toolsData := range all.FirstLevelOf() {
			targetPackage := pm.packages.GetOrCreatePackage(packager)

			for toolName, toolVersion := range toolsData.AsMap() {
				tool := targetPackage.GetOrCreateTool(toolName)
				version := semver.ParseRelaxed(toolVersion)
				release := tool.GetOrCreateRelease(version)
				release.InstallDir = toolPath
				pm.log.WithField("tool", release).Infof("Loaded tool")
			}
		}
	} else {
		// otherwise load the tools inside the unnamed package
		unnamedPackage := pm.packages.GetOrCreatePackage("")
		pm.LoadToolsFromPackageDir(unnamedPackage, toolsPath)
	}
	return nil
}

// LoadDiscoveries load all discoveries for all loaded platforms
// Returns error if:
// * A PluggableDiscovery instance can't be created
// * Tools required by the PlatformRelease cannot be found
// * Command line to start PluggableDiscovery has malformed or mismatched quotes
func (pme *Explorer) LoadDiscoveries() []error {
	var merr []error
	for _, platform := range pme.InstalledPlatformReleases() {
		merr = append(merr, pme.loadDiscoveries(platform)...)
	}
	merr = append(merr, pme.loadBuiltinDiscoveries()...)
	return merr
}

// loadDiscovery loads the discovery tool with id, if it cannot be found a non-nil status is returned
func (pme *Explorer) loadDiscovery(id string) error {
	tool := pme.GetTool(id)
	if tool == nil {
		return errors.New(tr("discovery %s not found", id))
	}
	toolRelease := tool.GetLatestInstalled()
	if toolRelease == nil {
		return errors.New(tr("discovery %s not installed", id))
	}
	discoveryPath := toolRelease.InstallDir.Join(tool.Name).String()
	d := discovery.New(id, discoveryPath)
	pme.discoveryManager.Add(d)
	return nil
}

// loadBuiltinDiscoveries loads the discovery tools that are part of the builtin package
func (pme *Explorer) loadBuiltinDiscoveries() []error {
	var merr []error
	for _, id := range []string{"builtin:serial-discovery", "builtin:mdns-discovery"} {
		if err := pme.loadDiscovery(id); err != nil {
			merr = append(merr, err)
		}
	}
	return merr
}

func (pme *Explorer) loadDiscoveries(release *cores.PlatformRelease) []error {
	var merr []error
	discoveryProperties := release.Properties.SubTree("pluggable_discovery")

	if discoveryProperties.Size() == 0 {
		return nil
	}

	// Handles discovery properties formatted like so:
	//
	// Case 1:
	//    "pluggable_discovery.required": "PLATFORM:DISCOVERY_NAME",
	//
	// Case 2:
	//    "pluggable_discovery.required.0": "PLATFORM:DISCOVERY_ID_1",
	//    "pluggable_discovery.required.1": "PLATFORM:DISCOVERY_ID_2",
	//
	// If both indexed and unindexed properties are found the unindexed are ignored
	for _, id := range discoveryProperties.ExtractSubIndexLists("required") {
		if err := pme.loadDiscovery(id); err != nil {
			merr = append(merr, err)
		}
	}

	discoveryIDs := discoveryProperties.FirstLevelOf()
	delete(discoveryIDs, "required")
	// Get the list of tools only if there are discoveries that use Direct discovery integration.
	// See:
	// https://arduino.github.io/arduino-cli/latest/platform-specification/#pluggable-discovery
	// We need the tools only in that case since we might need some tool's
	// runtime properties to expand the discovery pattern to run it correctly.
	var tools []*cores.ToolRelease
	if len(discoveryIDs) > 0 {
		var err error
		tools, err = pme.FindToolsRequiredFromPlatformRelease(release)
		if err != nil {
			merr = append(merr, err)
		}
	}

	// Handles discovery properties formatted like so:
	//
	// discovery.DISCOVERY_ID.pattern: "COMMAND_TO_EXECUTE"
	for discoveryID, props := range discoveryIDs {
		pattern, ok := props.GetOk("pattern")
		if !ok {
			merr = append(merr, errors.New(tr("can't find pattern for discovery with id %s", discoveryID)))
			continue
		}
		configuration := release.Properties.Clone()
		configuration.Merge(release.RuntimeProperties())
		configuration.Merge(props)

		for _, tool := range tools {
			configuration.Merge(tool.RuntimeProperties())
		}

		cmd := configuration.ExpandPropsInString(pattern)
		if cmdArgs, err := properties.SplitQuotedString(cmd, `"'`, true); err != nil {
			merr = append(merr, err)
		} else {
			d := discovery.New(discoveryID, cmdArgs...)
			pme.discoveryManager.Add(d)
		}
	}

	return merr
}
