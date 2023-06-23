package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	kustomv1 "github.com/fluxcd/kustomize-controller/api/v1beta2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

var scheme *runtime.Scheme

func init() {
	scheme = runtime.NewScheme()

	err := clientgoscheme.AddToScheme(scheme)
	if err != nil {
		panic(err.Error())
	}

	err = sourcev1.AddToScheme(scheme)
	if err != nil {
		panic(err.Error())
	}

	err = kustomv1.AddToScheme(scheme)
	if err != nil {
		panic(err)
	}
}

const (
	WARNING = 0
	INFO    = 1
	DEBUG   = 2
	TRACE   = 3
)

var log logr.Logger

func newLog(verbosity int) logr.Logger {
	stdr.SetVerbosity(verbosity)
	return stdr.New(nil)
}

func main() {
	global := &globalopts{}
	rootCmd := &cobra.Command{
		Use: "flux-whatif",
		PersistentPreRun: func(*cobra.Command, []string) {
			log = newLog(global.verbosity)
		},
	}
	global.addFlags(rootCmd)

	mo := &mergeopts{
		globalopts: global,
	}
	merge := &cobra.Command{
		Use:  "merge",
		RunE: mo.runE,
	}
	mo.addFlags(merge)

	rootCmd.AddCommand(merge)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

type globalopts struct {
	keepTmp   bool
	verbosity int
}

func (o *globalopts) addFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().BoolVar(&o.keepTmp, "keep", false, "if set, the temporary working directory will be logged and left intact rather than deleted")
	cmd.PersistentFlags().IntVarP(&o.verbosity, "verbosity", "v", 0, "verbosity level: -1=errors only, 0=warnings, 1=info messages, 2=debugging messages, 3=tracing messages")
}

type mergeopts struct {
	*globalopts
}

func (mo *mergeopts) addFlags(cmd *cobra.Command) {
}

func (mo *mergeopts) runE(cmd *cobra.Command, args []string) error {
	repoURL := args[0]
	targetBranch := args[1]
	newRef := args[2]

	cfg, err := config.GetConfig()
	if err != nil {
		return err
	}
	k8sClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	// usually the advice is: put values in a k/v pairs. Here I want to print out a simple statement; this is more UI than log message.
	fmt.Fprintf(os.Stderr, "What if git repo %q branch %q has HEAD %q\n\n", repoURL, targetBranch, newRef)

	ctx := context.Background()

	// List all the git repos, and find those with the particular repo
	// URL, and following the branch in question.
	var gitrepos sourcev1.GitRepositoryList
	if err = k8sClient.List(ctx, &gitrepos, &client.ListOptions{}); err != nil { // TODO namespace?
		return err
	}

	// keep a map, so we can look them up when finding Kustomizations that need to be applied.
	reposOfInterest := map[types.NamespacedName]*sourcev1.GitRepository{}

	for i := range gitrepos.Items {
		repo := &gitrepos.Items[i]
		if !repo.ObjectMeta.DeletionTimestamp.IsZero() {
			log.V(INFO).Info("warning: GitRepository is deleted; skipping", "name", client.ObjectKeyFromObject(repo))
			continue
		}
		if repo.Spec.Suspend {
			log.V(INFO).Info("warning: GitRepository is suspended; skipping", "name", client.ObjectKeyFromObject(repo))
			continue
		}
		if repo.Spec.URL == repoURL {
			branch := "master" // the default; TODO find a const for this
			if ref := repo.Spec.Reference; ref != nil {
				switch {
				case strings.HasPrefix(ref.Name, "refs/heads/%s"):
					// Name takes precedence over Tag, Branch and SemVer
					branch = strings.TrimPrefix(ref.Name, "refs/heads/")
				case ref.Tag != "" ||
					ref.Commit != "" ||
					ref.SemVer != "":
					// none of those would be affected by a branch
					// head changing
					log.V(DEBUG).Info("GitRepository does not track target branch; skipping", "name", client.ObjectKeyFromObject(repo), "branch", targetBranch)
					continue
				case ref.Branch != "":
					branch = ref.Branch
				}
				if branch == targetBranch {
					nsn := client.ObjectKeyFromObject(repo)
					log.V(INFO).Info("including GitRepository", "name", nsn)
					reposOfInterest[nsn] = repo
				} else {
					log.V(DEBUG).Info("GitRepository does not track target branch; skipping", "name", client.ObjectKeyFromObject(repo), "branch", targetBranch)
				}
			}
		}
	}

	// Find which Kustomizations depend on those
	var kustoms kustomv1.KustomizationList
	if err = k8sClient.List(ctx, &kustoms, &client.ListOptions{}); err != nil {
		return err
	}

	type kustomAndSource struct {
		kustom *kustomv1.Kustomization
		source *sourcev1.GitRepository
	}

	var kustomsToApply []kustomAndSource
	for i := range kustoms.Items {
		kustom := &kustoms.Items[i]
		// TODO check if ready i.e., viable?
		sourceRef := kustom.Spec.SourceRef
		if sourceRef.Kind == "GitRepository" { // FIXME APIVersion too
			repoName := types.NamespacedName{
				Name:      sourceRef.Name,
				Namespace: sourceRef.Namespace,
			}
			if repoName.Namespace == "" {
				repoName.Namespace = kustom.GetNamespace()
			}

			if src, ok := reposOfInterest[repoName]; ok {
				nsn := client.ObjectKeyFromObject(kustom)
				log.V(INFO).Info("including Kustomization using GitRepository", "name", client.ObjectKeyFromObject(kustom), "source name", nsn)
				kustomsToApply = append(kustomsToApply, kustomAndSource{
					kustom: kustom,
					source: src,
				})
			}
		}
	}

	// Simulate each of those with the content of the new branch

	//   Before starting, make a working space
	tmpRoot, err := os.MkdirTemp("", "flux-whatif-")
	if err != nil {
		return err
	}
	log.V(INFO).Info("temporary working directory created", "path", tmpRoot, "keep", mo.keepTmp)
	if !mo.keepTmp {
		defer os.RemoveAll(tmpRoot)
	}

	for _, ks := range kustomsToApply {
		//   Do the Kustomization dry-run, like `flux diff kustomization`,
		//   putting any changes to Flux objects onto a queue to be
		//   simulated.

		kustom := ks.kustom
		repo := ks.source

		artifactdir, err := ensureArtifactDir(ctx, tmpRoot, repo, newRef, k8sClient)
		if err != nil {
			return err
		}

		kustomizedir := filepath.Join(artifactdir, kustom.Spec.Path) // FIXME separators
		diff, err := dryrunKustomization(ctx, k8sClient, kustom, kustomizedir)
		if err != nil {
			return err
		}
		println(diff)
	}

	return nil
}

func ensureArtifactDir(ctx context.Context, tmp string, repo *sourcev1.GitRepository, newRef string, k8sClient client.Client) (artifact string, err error) {
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
	log.V(DEBUG).Info("attempting to clone", "url", repo.Spec.URL, "ref", newRef, "path", repodir)
	if err = fetchGitRepository(ctx, k8sClient, repodir, repo, newRef); err != nil {
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
