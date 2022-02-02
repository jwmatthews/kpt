// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package engine

import (
	"context"
	"fmt"
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	fnresultv1 "github.com/GoogleContainerTools/kpt/pkg/api/fnresult/v1"
	v1 "github.com/GoogleContainerTools/kpt/pkg/api/kptfile/v1"
	"github.com/GoogleContainerTools/kpt/pkg/fn"
	api "github.com/GoogleContainerTools/kpt/porch/api/porch/v1alpha1"
	configapi "github.com/GoogleContainerTools/kpt/porch/controllers/pkg/apis/porch/v1alpha1"
	"github.com/GoogleContainerTools/kpt/porch/engine/pkg/kpt"
	"github.com/GoogleContainerTools/kpt/porch/repository/pkg/cache"
	"github.com/GoogleContainerTools/kpt/porch/repository/pkg/repository"
	"sigs.k8s.io/kustomize/api/filesys"
	"sigs.k8s.io/kustomize/kyaml/kio"
)

type CaDEngine interface {
	OpenRepository(repositorySpec *configapi.Repository, auth repository.AuthOptions) (repository.Repository, error)
	CreatePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, auth repository.AuthOptions, obj *api.PackageRevision) (repository.PackageRevision, error)
	UpdatePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, auth repository.AuthOptions, oldPackage repository.PackageRevision, old, new *api.PackageRevision) (repository.PackageRevision, error)
	UpdatePackageResources(ctx context.Context, repositoryObj *configapi.Repository, auth repository.AuthOptions, oldPackage repository.PackageRevision, old, new *api.PackageRevisionResources) (repository.PackageRevision, error)
	DeletePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, auth repository.AuthOptions, obj repository.PackageRevision) error
	ListFunctions(ctx context.Context, repositoryObj *configapi.Repository, auth repository.AuthOptions) ([]repository.Function, error)
}

func NewCaDEngine(cache *cache.Cache) (CaDEngine, error) {
	return &cadEngine{
		cache:    cache,
		renderer: kpt.NewPlaceholderRenderer(),
		runner:   kpt.NewPlaceholderFunctionRunner(),
	}, nil
}

type cadEngine struct {
	cache    *cache.Cache
	renderer fn.Renderer
	runner   fn.FunctionRunner
}

var _ CaDEngine = &cadEngine{}

type mutation interface {
	Apply(ctx context.Context, resources repository.PackageResources) (repository.PackageResources, *api.Task, error)
}

func (cad *cadEngine) OpenRepository(repositorySpec *configapi.Repository, auth repository.AuthOptions) (repository.Repository, error) {
	return cad.cache.OpenRepository(repositorySpec, auth)
}

func (cad *cadEngine) CreatePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, auth repository.AuthOptions, obj *api.PackageRevision) (repository.PackageRevision, error) {
	repo, err := cad.cache.OpenRepository(repositoryObj, auth)
	if err != nil {
		return nil, err
	}
	draft, err := repo.CreatePackageRevision(ctx, obj)
	if err != nil {
		return nil, err
	}

	var mutations []mutation
	for i := range obj.Spec.Tasks {
		task := &obj.Spec.Tasks[i]
		switch task.Type {
		case api.TaskTypeClone:
			if task.Clone == nil {
				return nil, fmt.Errorf("clone not set for task of type %q", task.Type)
			}
			mutations = append(mutations, &clonePackageMutation{
				task: task,
				name: obj.Spec.PackageName,
			})

		case api.TaskTypePatch:
			if task.Patch == nil {
				return nil, fmt.Errorf("patch not set for task of type %q", task.Type)
			}
			// TODO: support patch?
			return nil, fmt.Errorf("patch not supported on create")

		case api.TaskTypeEval:
			if task.Eval == nil {
				return nil, fmt.Errorf("eval not set for task of type %q", task.Type)
			}
			mutations = append(mutations, &evalFunctionMutation{
				runner: cad.runner,
				task:   task,
			})

		default:
			return nil, fmt.Errorf("task of type %q not supported", task.Type)
		}
	}

	// Render package after creation.
	mutations = append(mutations, &renderPackageMutation{
		renderer: cad.renderer,
		runner:   cad.runner,
	})

	baseResources := repository.PackageResources{}

	return updateDraft(ctx, draft, baseResources, mutations)
}

func (cad *cadEngine) UpdatePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, auth repository.AuthOptions, oldPackage repository.PackageRevision, oldObj, newObj *api.PackageRevision) (repository.PackageRevision, error) {
	repo, err := cad.cache.OpenRepository(repositoryObj, auth)
	if err != nil {
		return nil, err
	}

	var mutations []mutation
	if len(oldObj.Spec.Tasks) != len(newObj.Spec.Tasks) {
		return nil, fmt.Errorf("adding/removing tasks is not yet supported")
	}

	for i := range oldObj.Spec.Tasks {
		oldTask := &oldObj.Spec.Tasks[i]
		newTask := &newObj.Spec.Tasks[i]

		if oldTask.Type != newTask.Type {
			return nil, fmt.Errorf("changing task types is not yet supported")
		}

		unchanged := reflect.DeepEqual(oldTask, newTask)
		if unchanged {
			continue
		}

		switch newTask.Type {
		case api.TaskTypeClone:
			if newTask.Clone == nil {
				return nil, fmt.Errorf("clone not set for task of type %q", newTask.Type)
			}
			if i != 0 {
				return nil, fmt.Errorf("clone only supported as first task")
			}
			mutation := &updatePackageMutation{
				task: newTask,
			}
			mutations = append(mutations, mutation)

		default:
			return nil, fmt.Errorf("updating task of type %q not supported", newTask.Type)
		}
	}

	mutations = append(mutations, &renderPackageMutation{
		renderer: cad.renderer,
		runner:   cad.runner,
	})

	draft, err := repo.UpdatePackage(ctx, oldPackage)
	if err != nil {
		return nil, err
	}

	apiResources, err := oldPackage.GetResources(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot get package resources: %w", err)
	}
	resources := repository.PackageResources{
		Contents: apiResources.Spec.Resources,
	}

	return updateDraft(ctx, draft, resources, mutations)
}

func (cad *cadEngine) DeletePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, auth repository.AuthOptions, oldPackage repository.PackageRevision) error {
	repo, err := cad.cache.OpenRepository(repositoryObj, auth)
	if err != nil {
		return err
	}

	if err := repo.DeletePackageRevision(ctx, oldPackage); err != nil {
		return err
	}

	return nil
}

func (cad *cadEngine) UpdatePackageResources(ctx context.Context, repositoryObj *configapi.Repository, auth repository.AuthOptions, oldPackage repository.PackageRevision, old, new *api.PackageRevisionResources) (repository.PackageRevision, error) {
	repo, err := cad.cache.OpenRepository(repositoryObj, auth)
	if err != nil {
		return nil, err
	}

	draft, err := repo.UpdatePackage(ctx, oldPackage)
	if err != nil {
		return nil, err
	}

	mutations := []mutation{
		&mutationReplaceResources{
			newResources: new,
			oldResources: old,
		},
	}

	apiResources, err := oldPackage.GetResources(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot get package resources: %w", err)
	}
	resources := repository.PackageResources{
		Contents: apiResources.Spec.Resources,
	}

	return updateDraft(ctx, draft, resources, mutations)
}

func updateDraft(ctx context.Context, draft repository.PackageDraft, baseResources repository.PackageResources, mutations []mutation) (repository.PackageRevision, error) {
	for _, m := range mutations {
		applied, task, err := m.Apply(ctx, baseResources)
		if err != nil {
			return nil, err
		}
		if err := draft.UpdateResources(ctx, &api.PackageRevisionResources{
			Spec: api.PackageRevisionResourcesSpec{
				Resources: applied.Contents,
			},
		}, task); err != nil {
			return nil, err
		}
		baseResources = applied
	}

	// Updates are done.
	return draft.Close(ctx)
}

func (cad *cadEngine) ListFunctions(ctx context.Context, repositoryObj *configapi.Repository, auth repository.AuthOptions) ([]repository.Function, error) {
	repo, err := cad.cache.OpenRepository(repositoryObj, auth)
	if err != nil {
		return nil, err
	}

	fns, err := repo.ListFunctions(ctx)
	if err != nil {
		return nil, err
	}

	return fns, nil
}

type updatePackageMutation struct {
	task *api.Task
}

func (m *updatePackageMutation) Apply(ctx context.Context, resources repository.PackageResources) (repository.PackageResources, *api.Task, error) {
	// TODO: load directly from source repository
	dir, err := ioutil.TempDir("", "kpt-pkg-update-*")
	if err != nil {
		return repository.PackageResources{}, nil, err
	}
	defer os.RemoveAll(dir)

	if err := writeResourcesToDirectory(dir, resources); err != nil {
		return repository.PackageResources{}, nil, err
	}

	ref := m.task.Clone.Upstream.Git.Ref

	// TODO: This is a hack
	packageName := filepath.Base(m.task.Clone.Upstream.Git.Directory)
	packageName = strings.TrimPrefix(packageName, ".git")

	packageDir := filepath.Join(dir, packageName)
	if err := kpt.PkgUpdate(ctx, ref, packageDir, kpt.PkgUpdateOpts{}); err != nil {
		return repository.PackageResources{}, nil, err
	}

	loaded, err := loadResourcesFromDirectory(dir)
	if err != nil {
		return repository.PackageResources{}, nil, err
	}

	return loaded, m.task, nil
}

func writeResourcesToDirectory(dir string, resources repository.PackageResources) error {
	for k, v := range resources.Contents {
		p := filepath.Join(dir, k)
		dir := filepath.Dir(p)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %q: %w", dir, err)
		}
		if err := os.WriteFile(p, []byte(v), 0644); err != nil {
			return fmt.Errorf("failed to write file %q: %w", dir, err)
		}
	}
	return nil
}

func loadResourcesFromDirectory(dir string) (repository.PackageResources, error) {
	// TODO: return abstraction instead of loading everything
	result := repository.PackageResources{
		Contents: map[string]string{},
	}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("cannot compute relative path %q, %q, %w", dir, path, err)
		}

		contents, err := ioutil.ReadFile(path)
		if err != nil {
			return fmt.Errorf("cannot read file %q: %w", dir, err)
		}
		result.Contents[rel] = string(contents)
		return nil
	}); err != nil {
		return repository.PackageResources{}, err
	}

	return result, nil
}

type evalFunctionMutation struct {
	runner fn.FunctionRunner
	task   *api.Task
}

func (m *evalFunctionMutation) Apply(ctx context.Context, resources repository.PackageResources) (repository.PackageResources, *api.Task, error) {
	e := m.task.Eval

	// TODO: Do this outside of Apply, Apply to take fs.
	fs := &kpt.MemFS{}
	if err := writeResources(fs, resources); err != nil {
		return repository.PackageResources{}, nil, err
	}

	results := fnresultv1.ResultList{}
	filter, err := m.runner.NewRunner(ctx, &v1.Function{
		Image:     e.Image,
		ConfigMap: e.ConfigMap,
		Selectors: []v1.Selector{},
	}, fn.RunnerOptions{
		ResultList: &results,
	})
	if err != nil {
		return repository.PackageResources{}, nil, fmt.Errorf("failed to create function runner: %w", err)
	}

	rw := &kio.LocalPackageReadWriter{
		PackagePath:        "/", // TODO: Populate with the package directory.
		IncludeSubpackages: true,
		FileSystem: filesys.FileSystemOrOnDisk{
			FileSystem: fs,
		},
	}

	pipeline := kio.Pipeline{
		Inputs:                []kio.Reader{rw},
		Filters:               []kio.Filter{filter},
		Outputs:               []kio.Writer{rw},
		ContinueOnEmptyResult: false,
	}

	if err := pipeline.Execute(); err != nil {
		return repository.PackageResources{}, nil, fmt.Errorf("failed to evaluate function: %w", err)
	}

	result, err := readResources(fs)
	if err != nil {
		return repository.PackageResources{}, nil, err
	}

	return result, m.task, nil
}

type mutationReplaceResources struct {
	newResources *api.PackageRevisionResources
	oldResources *api.PackageRevisionResources
}

func (m *mutationReplaceResources) Apply(ctx context.Context, resources repository.PackageResources) (repository.PackageResources, *api.Task, error) {
	patch := &api.PackagePatchTaskSpec{}
	task := &api.Task{
		Type:  "patch",
		Patch: patch,
	}

	old := resources.Contents
	new := m.newResources.Spec.Resources

	for k, newV := range new {
		oldV, ok := old[k]
		// New config or changed config
		if !ok || newV != oldV {
			patch.Patches = append(patch.Patches, k)
		}
	}
	for k := range old {
		// Deleted config
		if _, ok := new[k]; !ok {
			patch.Patches = append(patch.Patches, k)
		}
	}

	return repository.PackageResources{Contents: new}, task, nil
}
