package dockershim

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"

	"github.com/containers/image/v5/copy"
	istorage "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/podman/v3/pkg/rootless"
	"github.com/containers/storage"
)

// PullImage pulls an image with authentication config.
func (ds *dockerService) PullImage(_ context.Context, r *runtimeapi.PullImageRequest) (resp *runtimeapi.PullImageResponse, err error) {
	if r.Image == nil || r.Image.Image == "" {
		return nil, fmt.Errorf("[PullImage] Empty image: %v", r)
	}

	storeOpts, err := storage.DefaultStoreOptions(rootless.IsRootless(), rootless.GetRootlessUID())
	if err != nil {
		return nil, err
	}
	store, err := storage.GetStore(storeOpts)
	if err != nil {
		return nil, err
	}

	image := r.Image.Image

	// if r.Auth != nil {
	// 	username, password := r.Auth.Username, r.Auth.Password
	// 	if r.Auth.Auth != "" {
	// 		if username, password, err = decodeDockerAuth(r.Auth.Auth); err != nil {
	// 			klog.V(4).Infof("Error decoding authentication for image %s: %v", image, err)
	// 			return nil, err
	// 		}
	// 	}
	// }
	srcRef, err := alltransports.ParseImageName(image)
	destRef, err := istorage.Transport.ParseStoreReference(store, image)

	if _, err = copy.Image(nil, nil, destRef, srcRef, nil); err != nil {
		return nil, err
	}
	return nil, nil
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
