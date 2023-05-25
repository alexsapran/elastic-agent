// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package manifest

import (
	"context"
	"errors"
	"fmt"
	"github.com/elastic/elastic-agent/pkg/testing/tools"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/magefile/mage/mg"
	"golang.org/x/sync/errgroup"
)

// A backoff schedule for when and how often to retry failed HTTP
// requests. The first element is the time to wait after the
// first failure, the second the time to wait after the second
// failure, etc. After reaching the last element, retries stop
// and the request is considered failed.
var backoffSchedule = []time.Duration{
	1 * time.Second,
	3 * time.Second,
	10 * time.Second,
}

var invalidManifestURL = errors.New("invalid ManifestURL provided")
var notAllowedManifestURL = errors.New("the provided ManifestURL is not allowed URL")
var AllowedManifestHosts = []string{"snapshots.elastic.co", "staging.elastic.co"}

// DownloadManifest is going to download the given manifest file and return the ManifestResponse
func DownloadManifest(manifest string) (tools.Build, error) {
	manifestUrl, urlError := url.Parse(manifest)
	if urlError != nil {
		return tools.Build{}, invalidManifestURL
	}
	var valid = false
	for _, manifestHost := range AllowedManifestHosts {
		if manifestHost == manifestUrl.Host {
			valid = true
		}
	}
	if !valid {
		log.Printf("Not allowed %s, valid ones are %+v", manifestUrl.Host, AllowedManifestHosts)
		return tools.Build{}, notAllowedManifestURL
	}
	sanitizedUrl := fmt.Sprintf("https://%s%s", manifestUrl.Host, manifestUrl.Path)
	f := func() (tools.Build, error) { return downloadManifestData(sanitizedUrl) }
	manifestResponse, err := doWithRetries(f)
	if err != nil {
		return tools.Build{}, fmt.Errorf("downloading manifest: %w", err)
	}
	if mg.Verbose() {
		log.Printf(">>>> Downloaded manifest %s", manifest)
		log.Printf(">>>> Packaing version: %s, build_id: %s, manifest_version:%s", manifestResponse.Version, manifestResponse.BuildID, manifestResponse.ManifestVersion)
	}
	return manifestResponse, nil
}

func resolveManifestPackage(project tools.Project, pkg string, reqPackage string, version string) []string {
	packageName := fmt.Sprintf("%s-%s-%s", pkg, version, reqPackage)
	val, ok := project.Packages[packageName]
	if !ok {
		return nil
	}
	if mg.Verbose() {
		log.Printf(">>>>>>>>>>> Project branch/commit [%s, %s]", project.Branch, project.CommitHash)
	}
	return []string{val.URL, val.ShaURL, val.AscURL}

}

// DownloadComponentsFromManifest is going to download a set of components from the given manifest into the destination
// dropPath folder in order to later use that folder for packaging
func DownloadComponentsFromManifest(manifest string, platforms []string, platformPackages map[string]string, dropPath string) error {
	componentSpec := map[string][]string{
		"apm-server":            {"apm-server"},
		"beats":                 {"auditbeat", "filebeat", "heartbeat", "metricbeat", "osquerybeat", "packetbeat"},
		"cloud-defend":          {"cloud-defend"},
		"cloudbeat":             {"cloudbeat"},
		"elastic-agent-shipper": {"elastic-agent-shipper"},
		"endpoint-dev":          {"endpoint-security"},
		"fleet-server":          {"fleet-server"},
		"prodfiler":             {"pf-elastic-collector", "pf-elastic-symbolizer", "pf-host-agent"},
	}

	manifestResponse, err := DownloadManifest(manifest)
	if err != nil {
		return fmt.Errorf("failed to download remote manifest file %w", err)
	}
	projects := manifestResponse.Projects

	errGrp, downloadsCtx := errgroup.WithContext(context.Background())
	for component, pkgs := range componentSpec {
		for _, platform := range platforms {
			targetPath := filepath.Join(dropPath)
			err := os.MkdirAll(targetPath, 0755)
			if err != nil {
				return fmt.Errorf("failed to create directory %s", targetPath)
			}
			if mg.Verbose() {
				log.Printf(">>>>>>> Prepare to download project [%s] for [%s]", component, platform)
			}
			for _, pkg := range pkgs {
				reqPackage := platformPackages[platform]
				pkgURL := resolveManifestPackage(projects[component], pkg, reqPackage, manifestResponse.Version)
				if pkgURL != nil {
					for _, p := range pkgURL {
						log.Printf(">>>>>>>>> Downloading [%s] [%s] ", pkg, p)
						pkgFilename := path.Base(p)
						downloadTarget := filepath.Join(targetPath, pkgFilename)
						if _, err := os.Stat(downloadTarget); err != nil {
							func(downloadUrl string, target string) {
								parsedURL, errorUrl := url.Parse(downloadUrl)
								if errorUrl != nil {
									errGrp.Go(func() error { return invalidManifestURL })
								}
								var valid = false
								for _, manifestHost := range AllowedManifestHosts {
									if manifestHost == parsedURL.Host {
										valid = true
									}
								}
								if !valid {
									log.Printf("Not allowed %s, valid ones are %+v", parsedURL.Host, AllowedManifestHosts)
									errGrp.Go(func() error { return notAllowedManifestURL })
								}
								cleanUrl := fmt.Sprintf("https://%s%s", parsedURL.Host, parsedURL.Path)
								download := func() (string, error) { return downloadFile(downloadsCtx, cleanUrl, target) }
								errGrp.Go(func() error { _, err := doWithRetries(download); return err })
							}(p, downloadTarget)
						}
					}
				} else if mg.Verbose() {
					log.Printf(">>>>>>>>> Project [%s] does not have [%s] ", pkg, platform)
				}
			}
		}
	}

	err = errGrp.Wait()
	if err != nil {
		return fmt.Errorf("error downloading files: %w", err)
	}

	log.Printf("Downloads for manifest %q complete.", manifest)
	return nil
}
