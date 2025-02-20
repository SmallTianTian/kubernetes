//go:build !dockerless
// +build !dockerless

/*
Copyright 2016 The Kubernetes Authors.

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

package dockershim

import (
	"context"
	"fmt"
	"net/http"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/jsonmessage"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/dockershim/libdocker"
)

// This file implements methods in ImageManagerService.

// RemoveImage removes the image.
func (ds *dockerService) RemoveImage(_ context.Context, r *runtimeapi.RemoveImageRequest) (*runtimeapi.RemoveImageResponse, error) {
	image := r.GetImage()
	// If the image has multiple tags, we need to remove all the tags
	// TODO: We assume image.Image is image ID here, which is true in the current implementation
	// of kubelet, but we should still clarify this in CRI.
	imageInspect, err := ds.client.InspectImageByID(image.Image)

	// dockerclient.InspectImageByID doesn't work with digest and repoTags,
	// it is safe to continue removing it since there is another check below.
	if err != nil && !libdocker.IsImageNotFoundError(err) {
		return nil, err
	}

	if imageInspect == nil {
		// image is nil, assuming it doesn't exist.
		return &runtimeapi.RemoveImageResponse{}, nil
	}

	// An image can have different numbers of RepoTags and RepoDigests.
	// Iterating over both of them plus the image ID ensures the image really got removed.
	// It also prevents images from being deleted, which actually are deletable using this approach.
	var images []string
	images = append(images, imageInspect.RepoTags...)
	images = append(images, imageInspect.RepoDigests...)
	images = append(images, image.Image)

	for _, image := range images {
		if _, err := ds.client.RemoveImage(image, dockertypes.ImageRemoveOptions{PruneChildren: true}); err != nil && !libdocker.IsImageNotFoundError(err) {
			return nil, err
		}
	}

	return &runtimeapi.RemoveImageResponse{}, nil
}

// getImageRef returns the image digest if exists, or else returns the image ID.
func getImageRef(client libdocker.Interface, image string) (string, error) {
	img, err := client.InspectImageByRef(image)
	if err != nil {
		return "", err
	}
	if img == nil {
		return "", fmt.Errorf("unable to inspect image %s", image)
	}

	// Returns the digest if it exist.
	if len(img.RepoDigests) > 0 {
		return img.RepoDigests[0], nil
	}

	return img.ID, nil
}

func filterHTTPError(err error, image string) error {
	// docker/docker/pull/11314 prints detailed error info for docker pull.
	// When it hits 502, it returns a verbose html output including an inline svg,
	// which makes the output of kubectl get pods much harder to parse.
	// Here converts such verbose output to a concise one.
	jerr, ok := err.(*jsonmessage.JSONError)
	if ok && (jerr.Code == http.StatusBadGateway ||
		jerr.Code == http.StatusServiceUnavailable ||
		jerr.Code == http.StatusGatewayTimeout) {
		return fmt.Errorf("RegistryUnavailable: %v", err)
	}
	return err

}
