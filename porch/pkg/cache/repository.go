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

package cache

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleContainerTools/kpt/porch/api/porch/v1alpha1"
	"github.com/GoogleContainerTools/kpt/porch/pkg/repository"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/mod/semver"
	"k8s.io/klog/v2"
)

var tracer = otel.Tracer("cache")

type cachedRepository struct {
	id     string
	repo   repository.Repository
	cancel context.CancelFunc

	mutex          sync.Mutex
	cachedPackages []*cachedPackageRevision
	// TODO: Currently we support repositories with homogenous content (only packages xor functions). Model this more optimally?
	cachedFunctions []repository.Function
	// Error encountered on repository refresh by the refresh goroutine.
	// This is returned back by the cache to the background goroutine when it calls periodicall to resync repositories.
	refreshError error
}

// We take advantage of the cache having a global view of all the packages
// in a repository and compute the latest package revision in the cache
// rather than add another level of caching in the repositories themselves.
// This also reuses the revision comparison code and ensures same behavior
// between Git and OCI.
type cachedPackageRevision struct {
	repository.PackageRevision
	isLatestRevision bool
}

func (c *cachedPackageRevision) GetPackageRevision() *v1alpha1.PackageRevision {
	rev := c.PackageRevision.GetPackageRevision()
	if c.isLatestRevision {
		if rev.Labels == nil {
			rev.Labels = map[string]string{}
		}
		rev.Labels[v1alpha1.LatestPackageRevisionKey] = v1alpha1.LatestPackageRevisionValue
	}
	return rev
}

var _ repository.PackageRevision = &cachedPackageRevision{}

func newRepository(id string, repo repository.Repository) *cachedRepository {
	ctx, cancel := context.WithCancel(context.Background())
	r := &cachedRepository{
		id:     id,
		repo:   repo,
		cancel: cancel,
	}

	go r.pollForever(ctx)

	return r
}

var _ repository.Repository = &cachedRepository{}
var _ repository.FunctionRepository = &cachedRepository{}

func (r *cachedRepository) ListPackageRevisions(ctx context.Context, filter repository.ListPackageRevisionFilter) ([]repository.PackageRevision, error) {
	packages, err := r.getPackages(ctx, filter, false)
	if err != nil {
		return nil, err
	}

	return packages, nil
}

func (r *cachedRepository) ListFunctions(ctx context.Context) ([]repository.Function, error) {
	functions, err := r.getFunctions(ctx, false)
	if err != nil {
		return nil, err
	}
	return functions, nil
}

func (r *cachedRepository) getRefreshError() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.refreshError
}

func (r *cachedRepository) getPackages(ctx context.Context, filter repository.ListPackageRevisionFilter, forceRefresh bool) ([]repository.PackageRevision, error) {
	r.mutex.Lock()
	packages := r.cachedPackages
	err := r.refreshError
	r.mutex.Unlock()

	if forceRefresh {
		packages = nil
	}

	if packages == nil {
		// TODO: Avoid simultaneous fetches?
		// TODO: Push-down partial refresh?
		p, err := r.repo.ListPackageRevisions(ctx, repository.ListPackageRevisionFilter{})
		if err == nil {
			packages = toCachedPackageRevisionSlice(p)
		}

		r.mutex.Lock()
		r.cachedPackages = packages
		r.refreshError = err
		r.mutex.Unlock()
	}

	if err != nil {
		return nil, err
	}

	return toPackageRevisionSlice(packages, filter), nil
}

func (r *cachedRepository) getFunctions(ctx context.Context, force bool) ([]repository.Function, error) {
	var functions []repository.Function

	if !force {
		r.mutex.Lock()
		functions = r.cachedFunctions
		r.mutex.Unlock()
	}

	if functions == nil {
		fr, ok := (r.repo).(repository.FunctionRepository)
		if !ok {
			return []repository.Function{}, nil
		}

		if f, err := fr.ListFunctions(ctx); err != nil {
			return nil, err
		} else {
			functions = f
		}

		r.mutex.Lock()
		r.cachedFunctions = functions
		r.mutex.Unlock()
	}

	return functions, nil
}

func (r *cachedRepository) CreatePackageRevision(ctx context.Context, obj *v1alpha1.PackageRevision) (repository.PackageDraft, error) {
	created, err := r.repo.CreatePackageRevision(ctx, obj)
	if err != nil {
		return nil, err
	}

	return &cachedDraft{
		PackageDraft: created,
		cache:        r,
	}, nil
}

func (r *cachedRepository) UpdatePackage(ctx context.Context, old repository.PackageRevision) (repository.PackageDraft, error) {
	// Unwrap
	unwrapped := old.(*cachedPackageRevision).PackageRevision
	created, err := r.repo.UpdatePackage(ctx, unwrapped)
	if err != nil {
		return nil, err
	}

	return &cachedDraft{
		PackageDraft: created,
		cache:        r,
	}, nil
}

func (r *cachedRepository) update(closed repository.PackageRevision) *cachedPackageRevision {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	cached := &cachedPackageRevision{PackageRevision: closed}
	r.cachedPackages = updateOrAppend(r.cachedPackages, cached)
	// Recompute latest package revisions.
	identifyLatestRevisions(r.cachedPackages)
	return cached
}

func updateOrAppend(revisions []*cachedPackageRevision, new *cachedPackageRevision) []*cachedPackageRevision {
	for i, cached := range revisions {
		if cached.KubeObjectName() == new.KubeObjectName() {
			// TODO: more sophisticated conflict reconciliation?
			revisions[i] = new
			return revisions
		}
	}
	return append(revisions, new)
}

func (r *cachedRepository) DeletePackageRevision(ctx context.Context, old repository.PackageRevision) error {
	// Unwrap
	unwrapped := old.(*cachedPackageRevision).PackageRevision
	if err := r.repo.DeletePackageRevision(ctx, unwrapped); err != nil {
		return err
	}

	r.mutex.Lock()
	// TODO: Do something more efficient than a full cache flush
	r.cachedPackages = nil
	r.mutex.Unlock()

	return nil
}

func (r *cachedRepository) Close() error {
	r.cancel()
	return nil
}

// pollForever will continue polling until signal channel is closed or ctx is done.
func (r *cachedRepository) pollForever(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)

	for {
		select {
		case <-ticker.C:
			r.pollOnce(ctx)

		case <-ctx.Done():
			klog.V(2).Infof("exiting repository poller, because context is done: %v", ctx.Err())
			return
		}
	}
}

func (r *cachedRepository) pollOnce(ctx context.Context) {
	klog.Infof("background-refreshing repo %q", r.id)
	ctx, span := tracer.Start(ctx, "Repository::pollOnce", trace.WithAttributes())
	defer span.End()

	if _, err := r.getPackages(ctx, repository.ListPackageRevisionFilter{}, true); err != nil {
		klog.Warningf("error polling repo packages %s: %v", r.id, err)
	}
	if _, err := r.getFunctions(ctx, true); err != nil {
		klog.Warningf("error polling repo functions %s: %v", r.id, err)
	}
}

func toCachedPackageRevisionSlice(revisions []repository.PackageRevision) []*cachedPackageRevision {
	result := make([]*cachedPackageRevision, len(revisions))
	for i := range revisions {
		current := &cachedPackageRevision{
			PackageRevision:  revisions[i],
			isLatestRevision: false,
		}
		result[i] = current
	}
	identifyLatestRevisions(result)
	return result
}

func identifyLatestRevisions(result []*cachedPackageRevision) {
	// Compute the latest among the different revisions of the same package.
	// The map is keyed by the package name; Values are the latest revision found so far.
	latest := map[string]*cachedPackageRevision{}
	for _, current := range result {
		current.isLatestRevision = false // Clear all values

		// Check if the current package revision is more recent than the one seen so far.
		// Only consider Published packages
		if current.Lifecycle() != v1alpha1.PackageRevisionLifecyclePublished {
			continue
		}

		currentKey := current.Key()
		if previous, ok := latest[currentKey.Package]; ok {
			previousKey := previous.Key()
			switch cmp := semver.Compare(currentKey.Revision, previousKey.Revision); {
			case cmp == 0:
				// Same revision.
				klog.Warningf("Encountered package revisions whose versions compare equal: %q, %q", currentKey, previousKey)
			case cmp < 0:
				// currentKey.Revision < previousKey.Revision; no change
			case cmp > 0:
				// currentKey.Revision > previousKey.Revision; update latest
				latest[currentKey.Package] = current
			}
		} else if semver.IsValid(currentKey.Revision) {
			// First revision of the specific package; candidate for the latest.
			latest[currentKey.Package] = current
		}
	}
	// Mark the winners as latest
	for _, v := range latest {
		v.isLatestRevision = true
	}
}

func toPackageRevisionSlice(cached []*cachedPackageRevision, filter repository.ListPackageRevisionFilter) []repository.PackageRevision {
	result := make([]repository.PackageRevision, 0, len(cached))
	for _, p := range cached {
		if filter.Matches(p) {
			result = append(result, p)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		ki, kl := result[i].Key(), result[j].Key()
		switch res := strings.Compare(ki.Package, kl.Package); {
		case res < 0:
			return true
		case res > 0:
			return false
		default:
			// Equal. Compare next element
		}
		switch res := strings.Compare(ki.Revision, kl.Revision); {
		case res < 0:
			return true
		case res > 0:
			return false
		default:
			// Equal. Compare next element
		}
		switch res := strings.Compare(string(result[i].Lifecycle()), string(result[j].Lifecycle())); {
		case res < 0:
			return true
		case res > 0:
			return false
		default:
			// Equal. Compare next element
		}

		return strings.Compare(result[i].KubeObjectName(), result[j].KubeObjectName()) < 0
	})
	return result
}
