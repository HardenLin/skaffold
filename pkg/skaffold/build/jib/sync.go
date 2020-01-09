/*
Copyright 2019 The Skaffold Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package jib

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/pkg/errors"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/filemon"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
)

var syncLists = map[projectKey]SyncMap{}

type SyncMap map[string]SyncEntry

type SyncEntry struct {
	Dest     []string
	FileTime time.Time
	IsDirect bool
}

type JSONSyncMap struct {
	Direct    []JSONSyncEntry `json:"direct"`
	Generated []JSONSyncEntry `json:"generated"`
}

type JSONSyncEntry struct {
	Src  string `json:"src"`
	Dest string `json:"dest"`
}

func InitSync(ctx context.Context, workspace string, a *latest.JibArtifact) error {
	syncMap, err := getSyncMapFunc(ctx, workspace, a)
	if err != nil {
		return err
	}
	syncLists[getProjectKey(workspace, a)] = *syncMap
	return nil
}

// returns toCopy, toDelete, error
func GetSyncDiff(ctx context.Context, workspace string, a *latest.JibArtifact, e filemon.Events) (map[string][]string, map[string][]string, error) {
	// if anything that was modified was a buildfile, do NOT sync, do a rebuild
	buildFiles := GetBuildDefinitions(workspace, a)
	for _, f := range e.Modified {
		f, err := toAbs(f)
		if err != nil {
			return nil, nil, errors.WithStack(err)
		}
		for _, bf := range buildFiles {
			if f == bf {
				return nil, nil, nil
			}
		}
	}

	// no deletions
	if len(e.Deleted) != 0 {
		// change into logging
		fmt.Println("Deletions are not supported by jib auto sync at the moment")
		return nil, nil, nil
	}

	currSyncMap := syncLists[getProjectKey(workspace, a)]

	// if all files are modified and direct, we don't need to build anything
	if len(e.Deleted) == 0 && len(e.Added) == 0 {
		matches := make(map[string][]string)
		for _, f := range e.Modified {
			f, err := toAbs(f)
			if err != nil {
				return nil, nil, errors.WithStack(err)
			}
			if val, ok := currSyncMap[f]; ok {
				if !val.IsDirect {
					break
				}
				matches[f] = val.Dest
				// update file times in sync entries for these direct files, in case all matches are direct and we don't update the syncmap using a build
				infog, err := os.Stat(f)
				if err != nil {
					return nil, nil, errors.Wrap(err, "could not obtain file mod time")
				}
				val.FileTime = infog.ModTime()
				currSyncMap[f] = val
			} else {
				break
			}
		}
		if len(matches) == len(e.Modified) {
			return matches, nil, nil
		}
	}

	// we need to do another build and get a new sync map
	nextSyncMap, err := getSyncMapFunc(ctx, workspace, a)
	if err != nil {
		return nil, nil, err
	}
	syncLists[getProjectKey(workspace, a)] = *nextSyncMap

	fmt.Println("curr", currSyncMap)
	fmt.Println("next", nextSyncMap)

	toCopy := make(map[string][]string)
	// calculate the diff of the syncmaps
	for k, v := range *nextSyncMap {
		if curr, ok := currSyncMap[k]; ok {
			if v.FileTime != curr.FileTime {
				// file updated
				toCopy[k] = v.Dest
			}
		} else {
			// new file was created
			toCopy[k] = v.Dest
		}
	}

	return toCopy, nil, nil
}

// for testing
var (
	getSyncMapFunc = getSyncMap
)

func getSyncMap(ctx context.Context, workspace string, artifact *latest.JibArtifact) (*SyncMap, error) {
	// cmd will hold context that identifies the project
	cmd, err := getSyncMapCommand(ctx, workspace, artifact)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	sm, err := getSyncMapFromSystem(cmd)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return sm, nil
}

func getSyncMapCommand(ctx context.Context, workspace string, artifact *latest.JibArtifact) (*exec.Cmd, error) {
	t, err := DeterminePluginType(workspace, artifact)
	if err != nil {
		return nil, err
	}

	switch t {
	case JibMaven:
		return getSyncMapCommandMaven(ctx, workspace, artifact), nil
	case JibGradle:
		return getSyncMapCommandGradle(ctx, workspace, artifact), nil
	default:
		return nil, errors.Errorf("unable to determine Jib builder type for %s", workspace)
	}
}

func getSyncMapFromSystem(cmd *exec.Cmd) (*SyncMap, error) {
	jsm := JSONSyncMap{}
	stdout, err := util.RunCmdOut(cmd)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get Jib sync map")
	}

	// To parse the output, search for "BEGIN JIB JSON", then unmarshal the next line into the pathMap struct.
	// Syncmap is transitioning to "BEGIN JIB JSON: SYNCMAP/1" starting in jib 2.0.0
	// perhaps this feature should only be included from 2.0.0 onwards? And we generally avoid this?
	matches := regexp.MustCompile(`BEGIN JIB JSON(?:: SYNCMAP/1)?\r?\n({.*})`).FindSubmatch(stdout)
	if len(matches) == 0 {
		return nil, errors.New("failed to get Jib Sync data")
	}

	line := bytes.Replace(matches[1], []byte(`\`), []byte(`\\`), -1)
	if err := json.Unmarshal(line, &jsm); err != nil {
		return nil, errors.WithStack(err)
	}

	sm := make(SyncMap)
	for _, de := range jsm.Direct {
		info, err := os.Stat(de.Src)
		if err != nil {
			return nil, errors.Wrap(err, "could not obtain file mod time")
		}
		sm[de.Src] = SyncEntry{
			Dest:     []string{de.Dest},
			FileTime: info.ModTime(),
			IsDirect: true,
		}
	}
	for _, ge := range jsm.Generated {
		info, err := os.Stat(ge.Src)
		if err != nil {
			return nil, errors.Wrap(err, "could not obtain file mod time")
		}
		sm[ge.Src] = SyncEntry{
			Dest:     []string{ge.Dest},
			FileTime: info.ModTime(),
			IsDirect: false,
		}
	}
	return &sm, nil
}

func toAbs(f string) (string, error) {
	if !filepath.IsAbs(f) {
		af, err := filepath.Abs(f)
		if err != nil {
			return "", errors.WithStack(err)
		}
		return af, nil
	}
	return f, nil
}
