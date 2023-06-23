package main

import (
	"fmt"
	"os"

	kustomv1 "github.com/fluxcd/kustomize-controller/api/v1beta2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
