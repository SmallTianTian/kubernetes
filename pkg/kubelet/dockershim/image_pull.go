package dockershim

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog/v2"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/signature"
	istorage "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
)

// PullImage pulls an image with authentication config.
func (ds *dockerService) PullImage(ctx context.Context, r *runtimeapi.PullImageRequest) (resp *runtimeapi.PullImageResponse, err error) {
	if r.Image == nil || r.Image.Image == "" {
		return nil, fmt.Errorf("[PullImage] Empty image: %v", r)
	}

	image := r.Image.Image

	var username, password string
	if r.Auth != nil {
		username, password = r.Auth.Username, r.Auth.Password
		if r.Auth.Auth != "" {
			if username, password, err = decodeDockerAuth(r.Auth.Auth); err != nil {
				klog.V(4).Infof("Error decoding authentication for image %s: %v", image, err)
				return nil, err
			}
		}
	}
	sysCtx := &types.SystemContext{
		DockerAuthConfig: &types.DockerAuthConfig{
			Username: username,
			Password: password,
		},
	}
	srcRef, perr := alltransports.ParseImageName(image)
	if perr != nil {
		if ds.DefaultTransport == "" {
			return nil, perr
		}
		if srcRef, perr = alltransports.ParseImageName(ds.DefaultTransport + image); perr != nil {
			return nil, perr
		}
	}
	destRef, err := istorage.Transport.ParseStoreReference(ds.store, image)
	if err != nil {
		return nil, err
	}

	policy, err := signature.DefaultPolicy(sysCtx)
	if err != nil {
		return nil, err
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return nil, err
	}

	if _, err = copy.Image(ctx, policyContext, destRef, srcRef, &copy.Options{
		SourceCtx: sysCtx,
	}); err != nil {
		return nil, err
	}
	return &runtimeapi.PullImageResponse{
		ImageRef: destRef.DockerReference().String(),
	}, nil
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
