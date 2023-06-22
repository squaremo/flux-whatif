package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	kustomv1 "github.com/fluxcd/kustomize-controller/api/v1beta2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
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

func main() {
	rootCmd := &cobra.Command{
		Use: "flux-whatif",
	}
	global := &globalopts{}
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
	keepTmp bool
}

func (o *globalopts) addFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().BoolVar(&o.keepTmp, "keep", false, "if set, the temporary working directory will be logged and left intact rather than deleted")
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
		log.Fatal(err)
	}
	k8sClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("What if git repo %q branch %q has HEAD %q", repoURL, targetBranch, newRef)

	ctx := context.Background()

	// List all the git repos, and find those with the particular repo
	// URL, and following the branch in question.
	var gitrepos sourcev1.GitRepositoryList
	if err = k8sClient.List(ctx, &gitrepos, &client.ListOptions{}); err != nil { // TODO namespace?
		log.Fatal(err)
	}

	reposOfInterest := map[types.NamespacedName]*sourcev1.GitRepository{}

	for i := range gitrepos.Items {
		repo := &gitrepos.Items[i]
		if !repo.ObjectMeta.DeletionTimestamp.IsZero() {
			log.Printf("warning: GitRepository %s is deleted; skipping", client.ObjectKeyFromObject(repo))
			continue
		}
		if repo.Spec.Suspend {
			log.Printf("warning: GitRepository %s is suspended; skipping", client.ObjectKeyFromObject(repo))
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
					log.Printf("debug: GitRepository %s does not track branch %s; skipping", client.ObjectKeyFromObject(repo), targetBranch)
					continue
				case ref.Branch != "":
					branch = ref.Branch
				}
				if branch == targetBranch {
					nsn := client.ObjectKeyFromObject(repo)
					log.Printf("info: considering GitRepository %s", nsn)
					reposOfInterest[nsn] = repo
				} else {
					log.Printf("debug: GitRepository %s does not track branch %s; skipping", client.ObjectKeyFromObject(repo), targetBranch)
				}
			}
		}
	}

	// Find which Kustomizations depend on those
	var kustoms kustomv1.KustomizationList
	if err = k8sClient.List(ctx, &kustoms, &client.ListOptions{}); err != nil {
		log.Fatal(err)
	}

	var kustomsOfInterest []*kustomv1.Kustomization
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

			if _, ok := reposOfInterest[repoName]; ok {
				nsn := client.ObjectKeyFromObject(kustom)
				log.Printf("info: Found Kustomization %s using GitRepository %s", client.ObjectKeyFromObject(kustom), nsn)
				kustomsOfInterest = append(kustomsOfInterest, kustom)
			}
		}
	}

	// Simulate each of those with the content of the new branch

	//   Before starting, make a working space
	tmpRoot, err := os.MkdirTemp("", "flux-whatif-")
	if err != nil {
		log.Fatal(err)
	}
	if mo.keepTmp {
		log.Printf("info: local copy of git repos in %s", tmpRoot)
	} else {
		defer os.RemoveAll(tmpRoot)
	}

	//   First, clone each git repo at the new ref into the working space

	// TODO I should really only clone those that are used by a
	// Kustomization; others, we can record as unused.
	for nsn, repo := range reposOfInterest {
		repodir := filepath.Join(tmpRoot, "repo", nsn.Namespace, nsn.Name)
		if _, err := os.Stat(repodir); !os.IsNotExist(err) {
			continue
		}

		if err := os.MkdirAll(repodir, 0770); err != nil {
			return err
		}
		log.Printf("debug: attempting to clone %s at ref %s into %s", repo.Spec.URL, newRef, repodir)
		if err = fetchGitRepository(ctx, k8sClient, repodir, repo, newRef); err != nil {
			return err
		}

		//   Package it as each source does (this means I need to keep a
		//   tree above)

		artifactdir := filepath.Join(tmpRoot, "artifact", nsn.Namespace, nsn.Name)
		if err := os.MkdirAll(artifactdir, 0770); err != nil {
			return err
		}
		log.Printf("debug: constructing an artifact from %s according to %s", repodir, nsn)
		if err = constructArtifact(repodir, artifactdir, repo); err != nil {
			return err
		}
	}

	for _, kustom := range kustomsOfInterest {
		//   Do the Kustomization dry-run, like `flux diff kustomization`,
		//   putting any changes to Flux objects onto a queue to be
		//   simulated.

		name := kustom.Spec.SourceRef.Name
		namespace := kustom.GetNamespace()
		if ns := kustom.Spec.SourceRef.Namespace; ns != "" {
			namespace = ns
		}
		// factor this out
		artifactdir := filepath.Join(tmpRoot, "artifact", namespace, name)
		kustomizedir := filepath.Join(artifactdir, kustom.Spec.Path) // FIXME separators
		diff, err := dryrunKustomization(ctx, k8sClient, kustom, kustomizedir)
		if err != nil {
			return err
		}
		println(diff)
	}

	return nil
}

// https://github.com/fluxcd/flux2/blob/v2.0.0-rc.5/cmd/flux/diff_kustomization.go
