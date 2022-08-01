package dockershim

import (
	"context"
	"testing"
	"time"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/signature"
	istorage "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/podman/v3/pkg/rootless"
	"github.com/containers/storage"
	jsoniter "github.com/json-iterator/go"
	"k8s.io/klog"
)

func TestAbc(t *testing.T) {
	klog.InitFlags(nil)

	storeOpts, err := storage.DefaultStoreOptions(rootless.IsRootless(), rootless.GetRootlessUID())
	if err != nil {
		panic(err)
	}
	store, err := storage.GetStore(storeOpts)
	if err != nil {
		panic(err)
	}

	image := "docker://busybox"

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
	if err != nil {
		panic(err)
	}
	destRef, err := istorage.Transport.ParseStoreReference(store, "busybox")
	if err != nil {
		panic(err)
	}

	policy, err := signature.DefaultPolicy(nil)
	if err != nil {
		panic(err)
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		panic(err)
	}

	if _, err = copy.Image(context.Background(), policyContext, destRef, srcRef, &copy.Options{}); err != nil {
		panic(err)
	}
}

func TestAbd(t *testing.T) {
	var json = jsoniter.ConfigCompatibleWithStandardLibrary
	layers := []*storage.Layer{
		{
			ID:      "084326605ab6715ca698453e530e4d0319d4e402b468894a06affef944b4ef04",
			Created: time.Now(),
			Flags: map[string]interface{}{
				"incomplete": true,
			},
		},
	}
	t.Log(json.Marshal(layers))
}
