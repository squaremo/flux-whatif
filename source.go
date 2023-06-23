package main

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/fluxcd/pkg/git"
	"github.com/fluxcd/pkg/git/gogit"
	"github.com/fluxcd/pkg/git/repository"
	"github.com/fluxcd/pkg/sourceignore"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
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

// Adapted from
// https://github.com/fluxcd/source-controller/blob/main/internal/controller/gitrepository_controller.go#L620
func constructArtifact(repodir, artifactdir string, repo *sourcev1.GitRepository) error {
	ignoreDomain := strings.Split(repodir, string(filepath.Separator)) // TODO look at what this does; is it correct? harmless?
	ps, err := sourceignore.LoadIgnorePatterns(repodir, ignoreDomain)
	if err != nil {
		return err
	}
	if repo.Spec.Ignore != nil {
		ps = append(ps, sourceignore.ReadPatterns(strings.NewReader(*repo.Spec.Ignore), ignoreDomain)...)
	}
	return copyWithFilter(repodir, artifactdir, sourceIgnoreFilter(ps, ignoreDomain))
}

// This interface and func are copied, modulo renaming, from
// https://github.com/fluxcd/source-controller/blob/main/internal/controller/storage.go#L369-L381
// -- it could probably be in a pkg/ rather than internal. Here I use
// it just because it's a bit more convenient than using the
// sourceignore type.

type fileFilter func(string, os.FileInfo) bool

// sourceIgnoreFilter returns a fileFilter that filters out files
// matching sourceignore.VCSPatterns and any of the provided patterns.
// If an empty gitignore.Pattern slice is given, the matcher is set to
// sourceignore.NewDefaultMatcher.
func sourceIgnoreFilter(ps []gitignore.Pattern, domain []string) fileFilter {
	matcher := sourceignore.NewDefaultMatcher(ps, domain)
	if len(ps) > 0 {
		ps = append(sourceignore.VCSPatterns(domain), ps...)
		matcher = sourceignore.NewMatcher(ps)
	}
	return func(p string, fi os.FileInfo) bool {
		return matcher.Match(strings.Split(p, string(filepath.Separator)), fi.IsDir())
	}
}

// The analogue to this in source-controller/internal archives files
// as it goes, because it wants a tarball to serve at the end. I want
// a directory with files, so I'm going to copy them.
func copyWithFilter(repodir, artifactdir string, filter fileFilter) error {
	err := filepath.Walk(repodir, func(path string, info os.FileInfo, err error) error {
		// if we can't see some of the tree, abandon the whole thing
		if err != nil {
			return err
		}
		if path == repodir {
			return nil
		}
		if m := info.Mode(); !(m.IsRegular() || m.IsDir()) {
			return nil
		}
		if filter(path, info) {
			log.V(DEBUG).Info("skipping path as filtered", "path", path)
			if path != repodir && info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// bodge the path
		destpath := artifactdir + path[len(repodir):]
		log.V(DEBUG).Info("including file", "src", path, "dst", destpath)
		if info.IsDir() {
			return os.Mkdir(destpath, info.Mode())
		}
		// copy file
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		dst, err := os.Create(destpath)
		if err != nil {
			return err
		}
		defer dst.Close()
		_, err = io.Copy(dst, src)
		return err
	})
	return err
}
