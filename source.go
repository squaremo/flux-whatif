package main

import (
	"context"
	"net/url"

	"github.com/fluxcd/pkg/git"
	"github.com/fluxcd/pkg/git/gogit"
	"github.com/fluxcd/pkg/git/repository"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func fetchGitRepository(ctx context.Context, k8sClient client.Client, dir string, repo *sourcev1.GitRepository, newref string) error {
	// this is the relevant bit of the source-controller (sadly, but
	// understandably, it's internal):
	// https://github.com/fluxcd/source-controller/blob/v1.0.0-rc.5/internal/controller/gitrepository_controller.go#L466
	// Largely it is calls into fluxcd/pkg modules, surrounded by all
	// the bookkeeping you need for a controller. I've effectively
	// copied the guts over and taken the bookkeeping out.

	// Get the secret, which has all the auth info we'll need.
	var authData map[string][]byte
	if repo.Spec.SecretRef != nil {
		// Attempt to retrieve secret
		name := types.NamespacedName{
			Namespace: repo.GetNamespace(),
			Name:      repo.Spec.SecretRef.Name,
		}
		var secret corev1.Secret
		if err := k8sClient.Get(ctx, name, &secret); err != nil {
			return err
		}
		authData = secret.Data
	}

	u, err := url.Parse(repo.Spec.URL)
	if err != nil {
		return err
	}

	authOpts, err := git.NewAuthOptions(*u, authData)
	if err != nil {
		return err
	}

	// TODO: fetch artifacts from included sources; this would involve creating a tunnel to the Flux controller.
	// artifacts, err := fetchIncludes(k8sClient, ctx, obj)
	// ... up to L537

	_, err = gitCheckout(ctx, repo, newref, authOpts, dir)
	if err != nil {
		return err
	}

	// TODO verifying the signature L591
	// if result, err := r.verifyCommitSignature(ctx, obj, *commit); err != nil || result == sreconcile.ResultEmpty {
	// 	return result, err
	// }

	return nil
}

func gitCheckout(ctx context.Context,
	repo *sourcev1.GitRepository, newref string, authOpts *git.AuthOptions, dir string) (*git.Commit, error) {
	// this is from gitCheckout:
	// https://github.com/fluxcd/source-controller/blob/main/internal/controller/gitrepository_controller.go#L779,
	// with controller bookkeeping removed, and no logic changed.
	cloneOpts := repository.CloneConfig{
		RecurseSubmodules: repo.Spec.RecurseSubmodules,
		ShallowClone:      true,
	}
	// Changed: use the newref, rather than looking at the spec.
	cloneOpts.RefName = newref

	clientOpts := []gogit.ClientOption{gogit.WithDiskStorage()}
	if authOpts.Transport == git.HTTP {
		clientOpts = append(clientOpts, gogit.WithInsecureCredentialsOverHTTP())
	}

	gitReader, err := gogit.NewClient(dir, authOpts, clientOpts...)
	if err != nil {
		return nil, err
	}
	defer gitReader.Close()

	commit, err := gitReader.Clone(ctx, repo.Spec.URL, cloneOpts)
	return commit, err
}
