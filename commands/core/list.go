/*
 * This file is part of arduino-cli.
 *
 * Copyright 2018 ARDUINO SA (http://www.arduino.cc/)
 *
 * This software is released under the GNU General Public License version 3,
 * which covers the main part of arduino-cli.
 * The terms of this license can be found at:
 * https://www.gnu.org/licenses/gpl-3.0.en.html
 *
 * You can be released from the requirements of the above licenses by purchasing
 * a commercial license. Buying such a license is mandatory if you want to modify or
 * otherwise use the software for commercial activities involving the Arduino
 * software without disclosing the source code of your own applications. To purchase
 * a commercial license, send an email to license@arduino.cc.
 */

package core

import (
	"context"

	"github.com/arduino/arduino-cli/commands"
	"github.com/arduino/arduino-cli/rpc"
	"github.com/sirupsen/logrus"
	semver "go.bug.st/relaxed-semver"
)

func PlatformList(ctx context.Context, req *rpc.PlatformListReq, taskCB commands.TaskProgressCB) (*rpc.PlatformListResp, error) {
	logrus.Info("Executing `arduino core list`")
	pm := commands.GetPackageManager(req)
	taskCB(&rpc.TaskProgress{Name: "List installed platform", Completed: true})
	installed := []*rpc.InstalledPlatform{}
	for _, targetPackage := range pm.GetPackages().Packages {
		for _, platform := range targetPackage.Platforms {
			if platformRelease := pm.GetInstalledPlatformRelease(platform); platformRelease != nil {
				if req.GetUpdatableOnly() {
					if latest := platform.GetLatestRelease(); latest == nil || latest == platformRelease {
						continue
					}
				}
				var latestVersion *semver.Version
				if latest := platformRelease.Platform.GetLatestRelease(); latest != nil {
					latestVersion = latest.Version
				}
				installed = append(installed, &rpc.InstalledPlatform{
					ID:        platformRelease.String(),
					Installed: platformRelease.Version.String(),
					Latest:    latestVersion.String(),
					Name:      platformRelease.Platform.Name,
				})
			}
		}
	}

	// if len(installed) > 0 {
	// 	//formatter.Print(output.InstalledPlatforms{Platforms: installed})
	// }
	return &rpc.PlatformListResp{InstalledPlatform: installed}, nil
}
