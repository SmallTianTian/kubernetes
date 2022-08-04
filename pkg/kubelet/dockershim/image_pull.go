package dockershim

import (
	"context"
	"encoding/base64"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/pkg/shortnames"
	"github.com/containers/image/v5/signature"
	istorage "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	encconfig "github.com/containers/ocicrypt/config"
	"github.com/containers/storage"
	"github.com/docker/distribution/reference"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog/v2"
)

const (
	minimumTruncatedIDLength = 3
	localRegistryPrefix      = "localhost/"
	podCgroupName            = "pod"
)

var (
	ErrCannotParseImageID = fmt.Errorf("cannot parse an image ID")
)

// PullImage pulls an image with authentication config.
func (ds *dockerService) PullImage(ctx context.Context, r *runtimeapi.PullImageRequest) (resp *runtimeapi.PullImageResponse, err error) {
	image := r.GetImage().GetImage()
	sandboxCgroup := r.GetSandboxConfig().GetLinux().GetCgroupParent()
	klog.V(4).Infof("[PullImage] image: (%s), sandbox cgroup: (%s)", image, sandboxCgroup)
	if image == "" {
		return nil, fmt.Errorf("Empty image: %v", r)
	}

	pullArg := pullArguments{image: image, sandboxCgroup: sandboxCgroup}
	if r.Auth != nil {
		username, password := r.Auth.Username, r.Auth.Password
		if r.Auth.Auth != "" {
			if username, password, err = decodeDockerAuth(r.Auth.Auth); err != nil {
				return nil, fmt.Errorf("%w: Error decoding authentication(%s) for image (%s)", err, r.Auth.Auth, image)
			}
		}
		if username != "" {
			pullArg.credentials = types.DockerAuthConfig{Username: username, Password: password}
		}
	}

	// 快速释放锁。如果已经存在，则返回，不存在，则仅添加占用后返回
	pullOp, pullInProcess := func() (*pullOperation, bool) {
		ds.pullOperationsLock.Lock()
		defer ds.pullOperationsLock.Unlock()
		pullOp, inProgress := ds.pullOperationsInProgress[pullArg]
		if !inProgress {
			pullOp = &pullOperation{}
			pullOp.wg.Add(1)
			ds.pullOperationsInProgress[pullArg] = pullOp
		}
		return pullOp, inProgress
	}()

	if pullInProcess {
		// 既然其他线程已经在做同步了，这里就等着就好了
		pullOp.wg.Wait()
	} else {
		defer func() {
			pullOp.wg.Done()
			ds.pullOperationsLock.Lock()
			defer ds.pullOperationsLock.Unlock()
			delete(ds.pullOperationsInProgress, pullArg)
		}()
		pullOp.imageRef, pullOp.err = ds.pullImage(ctx, &pullArg)
	}
	return &runtimeapi.PullImageResponse{ImageRef: pullOp.imageRef}, pullOp.err
}

type pullArguments struct {
	image         string
	sandboxCgroup string
	credentials   types.DockerAuthConfig
}

type pullOperation struct {
	wg       sync.WaitGroup
	imageRef string
	err      error
}

type imageCopyOptions struct {
	store  storage.Store
	srcRef types.ImageReference
	cgroup string
	// SourceCtx        *types.SystemContext
	DestinationCtx   *types.SystemContext
	OciDecryptConfig *encconfig.DecryptConfig
	ProgressInterval time.Duration
	Progress         chan types.ProgressProperties `json:"-"`
}

func decodeDockerAuth(s string) (user, password string, _ error) {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", "", err
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		// if it's invalid just skip, as docker does
		return "", "", nil
	}
	user = parts[0]
	password = strings.Trim(parts[1], "\x00")
	return user, password, nil
}

func (ds *dockerService) pullImage(ctx context.Context, pullArg *pullArguments) (imageRef string, err error) {
	defer func() {
		if pe := recover(); pe != nil {
			bs := debug.Stack()
			err = fmt.Errorf("pullImage was aborted by a Go panic: %v\n%s", pe, string(bs))
		}
	}()

	sourceCtx := &types.SystemContext{}
	sourceCtx.DockerLogMirrorChoice = true // Add info level log of the pull source
	if pullArg.credentials.Username != "" {
		sourceCtx.DockerAuthConfig = &pullArg.credentials
	}

	var pulled string
	images, re := resolveNames(sourceCtx, ds.store, pullArg.image)
	if re != nil {
		return "", fmt.Errorf("%w: ResolveNames failed", re)
	}
	for _, img := range images {
		srcRef, err := prepareImageReference(img, ds.DefaultTransport)
		if err != nil {
			klog.V(4).InfoS("Error prepareImageReference image: %s: %v", img, err)
			continue
		}
		tmpImg, err := srcRef.NewImage(context.Background(), sourceCtx)
		if err != nil {
			if strings.HasPrefix(img, localRegistryPrefix) {
				if _, le := ds.imageStatus(sourceCtx, img); le == nil {
					pulled = img
					break
				}
			}
			klog.V(4).InfoS("Error preparing image: %s: %v", img, err)
			continue
		}
		defer tmpImg.Close() // nolint:gocritic

		var storedImage *imageResult
		storedImage, err = ds.imageStatus(sourceCtx, img)
		if err == nil {
			tmpImgConfigDigest := tmpImg.ConfigInfo().Digest
			if tmpImgConfigDigest.String() == "" {
				klog.V(4).InfoS("Image config digest is empty, re-pulling image")
			} else if tmpImgConfigDigest.String() == storedImage.ConfigDigest.String() {
				klog.V(4).InfoS("Image %s already in store, skipping pull", img)
				pulled = img
				break
			}
			klog.V(4).InfoS("Image in store has different ID, re-pulling %s", img)
		}

		progress := make(chan types.ProgressProperties)
		defer close(progress) // nolint:gocritic
		go func() {
			for p := range progress {
				if p.Artifact.Size > 0 {
					fmt.Printf("ImagePull (%v): %s (%s): %v bytes (%.2f%%)",
						p.Event, img, p.Artifact.Digest, p.Offset,
						float64(p.Offset)/float64(p.Artifact.Size)*100,
					)
				} else {
					fmt.Printf("ImagePull (%v): %s (%s): %v bytes",
						p.Event, img, p.Artifact.Digest, p.Offset,
					)
				}
			}
		}()

		cgroup := ""
		if ds.separatePullCgroup != "" {
			if ds.separatePullCgroup == podCgroupName {
				cgroup = pullArg.sandboxCgroup
			} else if cgroup = ds.separatePullCgroup; !strings.Contains(cgroup, ".slice") {
				return "", fmt.Errorf("invalid systemd cgroup %q", cgroup)
			}
		}

		if err = _realPullImage(sourceCtx, img, &imageCopyOptions{
			store:            ds.store,
			srcRef:           srcRef,
			cgroup:           cgroup,
			DestinationCtx:   sourceCtx,
			ProgressInterval: time.Second,
			Progress:         progress,
		}); err != nil {
			klog.V(4).InfoS("Error pulling image %s: %v", img, err)
			continue
		}
		pulled = img
		break
	}

	if pulled == "" && err != nil {
		return "", err
	}
	status, err := ds.imageStatus(sourceCtx, pulled)
	if err != nil {
		return "", err
	}
	fmt.Println(status)
	imageRef = status.ID
	if len(status.RepoDigests) > 0 {
		imageRef = status.RepoDigests[0]
	}
	return
}

func resolveNames(systemContext *types.SystemContext, store storage.Store, imageName string) ([]string, error) {
	// _Maybe_ it's a truncated image ID.  Don't prepend a registry name, then.
	if len(imageName) >= minimumTruncatedIDLength && store != nil {
		if img, err := store.Image(imageName); err == nil && img != nil && strings.HasPrefix(img.ID, imageName) {
			// It's a truncated version of the ID of an image that's present in local storage;
			// we need to expand it.
			return []string{img.ID}, nil
		}
	}
	// This to prevent any image ID to go through this routine
	_, err := reference.ParseNormalizedNamed(imageName)
	if err != nil {
		if strings.Contains(err.Error(), "cannot specify 64-byte hexadecimal strings") {
			return nil, ErrCannotParseImageID
		}
		return nil, fmt.Errorf("%w: ParseNormalizedNamed failed", err)
	}

	// Disable short name alias mode. Will enable it once we settle on a shortname alias table.
	disabled := types.ShortNameModeDisabled
	systemContext.ShortNameMode = &disabled
	resolved, err := shortnames.Resolve(systemContext, imageName)
	if err != nil {
		return nil, fmt.Errorf("%w: shortnames resolve failed", err)
	}

	if desc := resolved.Description(); len(desc) > 0 {
		klog.V(5).Info(desc)
	}

	images := make([]string, len(resolved.PullCandidates))
	for i := range resolved.PullCandidates {
		// Strip the tag from ambiguous image references that have a
		// digest as well (e.g.  `image:tag@sha256:123...`).  Such
		// image references are supported by docker but, due to their
		// ambiguity, explicitly not by containers/image.
		ref := resolved.PullCandidates[i].Value
		_, isTagged := ref.(reference.NamedTagged)
		canonical, isDigested := ref.(reference.Canonical)
		if isTagged && isDigested {
			canonical, err = reference.WithDigest(reference.TrimNamed(ref), canonical.Digest())
			if err != nil {
				return nil, err
			}
			ref = canonical
		}
		images[i] = ref.String()
	}
	return images, nil
}

func prepareImageReference(imageName, defaultTransport string) (types.ImageReference, error) {
	srcRef, err := alltransports.ParseImageName(imageName)
	if err != nil {
		if defaultTransport != "" {
			srcRef, err = alltransports.ParseImageName(defaultTransport + imageName)
		}
	}
	return srcRef, err
}

func _realPullImage(sysCtx *types.SystemContext, imageName string, options *imageCopyOptions) error {
	dest := imageName
	if options.srcRef.DockerReference() != nil {
		dest = options.srcRef.DockerReference().String()
	}

	destRef, err := istorage.Transport.ParseStoreReference(options.store, dest)
	if err != nil {
		return err
	}

	if options.cgroup != "" {
		err = fmt.Errorf("NOT SUPPORT: pull image in new cgroup") // TODO：暂时不支持在新 cgroup 中拉取镜像
	} else {
		var policy *signature.Policy
		if policy, err = signature.DefaultPolicy(sysCtx); err != nil {
			return err
		}
		var policyContext *signature.PolicyContext
		if policyContext, err = signature.NewPolicyContext(policy); err != nil {
			return err
		}

		_, err = copy.Image(context.Background(), policyContext, destRef, options.srcRef, &copy.Options{
			DestinationCtx:   options.DestinationCtx,
			OciDecryptConfig: options.OciDecryptConfig,
			ProgressInterval: options.ProgressInterval,
			Progress:         options.Progress,
		})
	}
	return err
}
