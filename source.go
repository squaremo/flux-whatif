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

// Find the Repos that use the URL given, are active, and pass
// `filter`. At present, `filter` should mutate the object to reflect
// the particular scenario.
func findRepos(ctx context.Context, k8sClient client.Client, url string, filter func(*sourcev1.GitRepository) bool) (sourceMap, error) {
	// List all the git repos, and find those with the particular repo
	// URL, and following the branch in question. Mutate them to use
	// the new ref, so an artifact is constructed from the commit per
	// the scenario.
	var gitrepos sourcev1.GitRepositoryList
	if err := k8sClient.List(ctx, &gitrepos, &client.ListOptions{}); err != nil { // TODO namespace?
		return nil, err
	}

	// keep a map, so we can look them up when finding Kustomizations that need to be applied.
	reposOfInterest := sourceMap{}

	for i := range gitrepos.Items {
		repo := &gitrepos.Items[i]

		if !repo.ObjectMeta.DeletionTimestamp.IsZero() {
			log.V(INFO).Info("GitRepository is deleted; skipping", "name", client.ObjectKeyFromObject(repo))
			continue
		}

		if repo.Spec.URL != url {
			log.V(DEBUG).Info("GitRepository does not match URL; skipping", "name", client.ObjectKeyFromObject(repo))
			continue
		}

		if repo.Spec.Suspend {
			log.V(INFO).Info("GitRepository is suspended; skipping", "name", client.ObjectKeyFromObject(repo))
			continue
		}

		if filter(repo) {
			reposOfInterest[client.ObjectKeyFromObject(repo)] = repo
		}
	}
	return reposOfInterest, nil
}

func fetchGitRepository(ctx context.Context, k8sClient client.Client, dir string, repo *sourcev1.GitRepository) error {
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

	_, err = gitCheckout(ctx, repo, authOpts, dir)
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
	repo *sourcev1.GitRepository, authOpts *git.AuthOptions, dir string) (*git.Commit, error) {
	// this is from gitCheckout:
	// https://github.com/fluxcd/source-controller/blob/main/internal/controller/gitrepository_controller.go#L779,
	// with controller bookkeeping removed, and no logic changed.
	cloneOpts := repository.CloneConfig{
		RecurseSubmodules: repo.Spec.RecurseSubmodules,
		ShallowClone:      true,
	}
	if ref := repo.Spec.Reference; ref != nil {
		cloneOpts.Branch = ref.Branch
		cloneOpts.Commit = ref.Commit
		cloneOpts.Tag = ref.Tag
		cloneOpts.SemVer = ref.SemVer
		cloneOpts.RefName = ref.Name
	}

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

func ensureArtifactDir(ctx context.Context, tmp string, k8sClient client.Client, repo *sourcev1.GitRepository) (artifact string, err error) {
	repodir := filepath.Join(tmp, "repo", repo.GetNamespace(), repo.GetName())
	artifactdir := filepath.Join(tmp, "artifact", repo.GetNamespace(), repo.GetName())

	// Assume if the artifact dir has been created, all the packaging
	// bit succeeded.
	if _, err := os.Stat(artifactdir); err == nil {
		return artifactdir, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	// Assume if the artifact hasn't been made, neither has the repo
	// been cloned.
	if err := os.MkdirAll(repodir, 0770); err != nil {
		return "", err
	}
	log.V(DEBUG).Info("attempting to clone", "url", repo.Spec.URL, "ref", repo.Spec.Reference, "path", repodir)
	if err = fetchGitRepository(ctx, k8sClient, repodir, repo); err != nil {
		return "", err
	}

	//   Package it as each source does (this means I need to keep a
	//   tree above)

	if err := os.MkdirAll(artifactdir, 0770); err != nil {
		return "", err
	}
	log.V(DEBUG).Info("constructing an artifact from repo", "path", artifactdir, "source name", client.ObjectKeyFromObject(repo))
	if err = constructArtifact(repodir, artifactdir, repo); err != nil {
		return "", err
	}

	return artifactdir, err
}
