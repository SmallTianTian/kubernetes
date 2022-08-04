package dockershim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	dref "github.com/containers/image/v5/docker/reference"
	istorage "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog/v2"
)

// PullImage pulls an image with authentication config.
func (ds *dockerService) ImageStatus(_ context.Context, r *runtimeapi.ImageStatusRequest) (*runtimeapi.ImageStatusResponse, error) {
	var resp *runtimeapi.ImageStatusResponse
	image := r.GetImage().GetImage()
	klog.V(4).Infof("[ImageStatus] image: (%s)", image)
	if image == "" {
		return nil, fmt.Errorf("no image specified")
	}

	sysCtx := &types.SystemContext{}
	images, err := resolveNames(sysCtx, ds.store, image)
	if err != nil {
		if err != ErrCannotParseImageID {
			return nil, err
		}
		images = append(images, image)
	}
	var (
		notfound bool
		lastErr  error
	)
	for _, image := range images {
		status, err := ds.imageStatus(sysCtx, image)
		if err != nil {
			if errors.Is(err, storage.ErrImageUnknown) {
				klog.V(5).Infof("Can't find %s", image)
				notfound = true
				continue
			}
			klog.V(3).Infof("Error getting status from %s: %v", image, err)
			lastErr = err
			continue
		}

		// Ensure that size is already defined
		var size uint64
		if status.Size == nil {
			size = 0
		} else {
			size = *status.Size
		}

		resp = &runtimeapi.ImageStatusResponse{
			Image: &runtimeapi.Image{
				Id:          status.ID,
				RepoTags:    status.RepoTags,
				RepoDigests: status.RepoDigests,
				Size_:       size,
			},
		}
		if r.Verbose {
			info, err := createImageInfo(status)
			if err != nil {
				return nil, fmt.Errorf("creating image info: %w", err)
			}
			resp.Info = info
		}
		uid, username := getUserFromImage(status.User)
		if uid != nil {
			resp.Image.Uid = &runtimeapi.Int64Value{Value: *uid}
		}
		resp.Image.Username = username
		break
	}
	if lastErr != nil && resp == nil {
		return nil, lastErr
	}
	if notfound && resp == nil {
		klog.V(5).Infof("Image %s not found", image)
		return &runtimeapi.ImageStatusResponse{}, nil
	}

	klog.V(4).Infof("Image status: %v", resp)
	return resp, nil
}

func (ds *dockerService) imageStatus(systemContext *types.SystemContext, nameOrID string) (*imageResult, error) {
	ref, err := getRef(ds.store, nameOrID)
	if err != nil {
		return nil, err
	}
	image, err := istorage.Transport.GetStoreImage(ds.store, ref)
	if err != nil {
		return nil, err
	}
	ds.imageCacheLock.Lock()
	cacheItem, ok := ds.imageCache[image.ID]
	ds.imageCacheLock.Unlock()

	if !ok {
		cacheItem, err = buildImageCacheItem(systemContext, ref) // Single-use-only, not actually cached
		if err != nil {
			return nil, err
		}
	}

	result := buildImageResult(ds.store, image, cacheItem)
	return &result, nil
}

func getRef(store storage.Store, name string) (types.ImageReference, error) {
	ref, err := alltransports.ParseImageName(name)
	if err == nil {
		return ref, nil
	}
	ref, err = istorage.Transport.ParseStoreReference(store, "@"+name)
	if err == nil {
		return ref, nil
	}
	return istorage.Transport.ParseStoreReference(store, name)
}

type imageCacheItem struct {
	config       *specs.Image
	size         *uint64
	configDigest digest.Digest
	info         *types.ImageInspectInfo
}

type imageResult struct {
	ID           string
	Name         string
	RepoTags     []string
	RepoDigests  []string
	Size         *uint64
	Digest       digest.Digest
	ConfigDigest digest.Digest
	User         string
	PreviousName string
	Labels       map[string]string
	OCIConfig    *specs.Image
}

func buildImageCacheItem(systemContext *types.SystemContext, ref types.ImageReference) (imageCacheItem, error) {
	ctx := context.Background()
	imageFull, err := ref.NewImage(ctx, systemContext)
	if err != nil {
		return imageCacheItem{}, err
	}
	defer imageFull.Close()
	configDigest := imageFull.ConfigInfo().Digest
	imageConfig, err := imageFull.OCIConfig(ctx)
	if err != nil {
		return imageCacheItem{}, err
	}
	size, err := imageFull.Size()
	if err != nil {
		size = 0
	}
	uSize := uint64(size)

	info, err := imageFull.Inspect(ctx)
	if err != nil {
		return imageCacheItem{}, fmt.Errorf("inspecting image: %w", err)
	}

	return imageCacheItem{
		config:       imageConfig,
		size:         &uSize,
		configDigest: configDigest,
		info:         info,
	}, nil
}

func buildImageResult(store storage.Store, image *storage.Image, cacheItem imageCacheItem) imageResult {
	name, tags, digests := sortNamesByType(image.Names)
	imageDigest, repoDigests := makeRepoDigests(store, digests, tags, image)
	sort.Strings(tags)
	sort.Strings(repoDigests)
	previousName := ""
	if len(image.NamesHistory) > 0 {
		// Remove the tag because we can only keep the name as indicator
		split := strings.SplitN(image.NamesHistory[0], ":", 2)
		if len(split) > 0 {
			previousName = split[0]
		}
	}

	return imageResult{
		ID:           image.ID,
		Name:         name,
		RepoTags:     tags,
		RepoDigests:  repoDigests,
		Size:         cacheItem.size,
		Digest:       imageDigest,
		ConfigDigest: cacheItem.configDigest,
		User:         cacheItem.config.Config.User,
		PreviousName: previousName,
		Labels:       cacheItem.info.Labels,
		OCIConfig:    cacheItem.config,
	}
}

func makeRepoDigests(store storage.Store, knownRepoDigests, tags []string, img *storage.Image) (imageDigest digest.Digest, repoDigests []string) {
	// Look up the image's digests.
	imageDigest = img.Digest
	if imageDigest == "" {
		imgDigest, err := store.ImageBigDataDigest(img.ID, storage.ImageDigestBigDataKey)
		if err != nil || imgDigest == "" {
			return "", knownRepoDigests
		}
		imageDigest = imgDigest
	}
	imageDigests := []digest.Digest{imageDigest}
	for _, anotherImageDigest := range img.Digests {
		if anotherImageDigest != imageDigest {
			imageDigests = append(imageDigests, anotherImageDigest)
		}
	}
	// We only want to supplement what's already explicitly in the list, so keep track of values
	// that we already know.
	digestMap := make(map[string]struct{})
	repoDigests = knownRepoDigests
	for _, repoDigest := range knownRepoDigests {
		digestMap[repoDigest] = struct{}{}
	}
	// For each tagged name, parse the name, and if we can extract a named reference, convert
	// it into a canonical reference using the digest and add it to the list.
	for _, name := range append(tags, knownRepoDigests...) {
		if ref, err2 := dref.ParseNormalizedNamed(name); err2 == nil {
			trimmed := dref.TrimNamed(ref)
			for _, imageDigest := range imageDigests {
				if imageRef, err3 := dref.WithDigest(trimmed, imageDigest); err3 == nil {
					if _, ok := digestMap[imageRef.String()]; !ok {
						repoDigests = append(repoDigests, imageRef.String())
						digestMap[imageRef.String()] = struct{}{}
					}
				}
			}
		}
	}
	return imageDigest, repoDigests
}

func getUserFromImage(user string) (id *int64, username string) {
	// return both empty if user is not specified in the image.
	if user == "" {
		return nil, ""
	}
	// split instances where the id may contain user:group
	user = strings.Split(user, ":")[0]
	// user could be either uid or user name. Try to interpret as numeric uid.
	uid, err := strconv.ParseInt(user, 10, 64)
	if err != nil {
		// If user is non numeric, assume it's user name.
		return nil, user
	}
	// If user is a numeric uid.
	return &uid, ""
}

func createImageInfo(result *imageResult) (map[string]string, error) {
	info := struct {
		Labels    map[string]string `json:"labels,omitempty"`
		ImageSpec *specs.Image      `json:"imageSpec"`
	}{
		result.Labels,
		result.OCIConfig,
	}
	bytes, err := json.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("marshal data: %v: %w", info, err)
	}
	return map[string]string{"info": string(bytes)}, nil
}
