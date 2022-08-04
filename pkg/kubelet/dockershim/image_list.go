package dockershim

import (
	"context"

	istorage "github.com/containers/image/v5/storage"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// ListImages lists existing images.
func (ds *dockerService) ListImages(ctx context.Context, r *runtimeapi.ListImagesRequest) (*runtimeapi.ListImagesResponse, error) {
	filter := r.GetFilter()
	if filter != nil {
		if filter.GetImage().GetImage() != "" {
			return nil, nil
		}
	}
	images, err := ds.store.Images()
	if err != nil {
		return nil, err
	}

	var result []*runtimeapi.Image
	for _, image := range images {
		_, tags, digests := sortNamesByType(image.Names)
		ref, err := istorage.Transport.ParseStoreReference(ds.store, "@"+image.ID)
		if err != nil {
			return nil, err
		}

		ic, err := ref.NewImage(ctx, nil)
		if err != nil {
			return nil, err
		}

		size, err := ic.Size()
		if err != nil {
			return nil, err
		}

		result = append(result, &runtimeapi.Image{
			Id:          image.ID,
			RepoTags:    tags,
			RepoDigests: digests,
			Size_:       uint64(size),
			// Uid:         image.Uid,
			// Username:    image.Username,
			// Spec:        image.Spec,
		})
	}

	return &runtimeapi.ListImagesResponse{Images: result}, nil
}

func sortNamesByType(names []string) (bestName string, tags, digests []string) {
	for _, name := range names {
		if len(name) > 72 && name[len(name)-72:len(name)-64] == "@sha256:" {
			digests = append(digests, name)
		} else {
			tags = append(tags, name)
		}
	}
	if len(digests) > 0 {
		bestName = digests[0]
	}
	if len(tags) > 0 {
		bestName = tags[0]
	}
	return bestName, tags, digests
}
