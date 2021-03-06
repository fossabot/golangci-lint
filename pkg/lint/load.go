package lint

import (
	"context"
	"fmt"
	"go/build"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golangci/tools/go/ssa"
	"github.com/golangci/tools/go/ssa/ssautil"
	"github.com/pkg/errors"
	"golang.org/x/tools/go/loader"
	"golang.org/x/tools/go/packages"

	"github.com/golangci/golangci-lint/pkg/config"
	"github.com/golangci/golangci-lint/pkg/exitcodes"
	"github.com/golangci/golangci-lint/pkg/goutil"
	"github.com/golangci/golangci-lint/pkg/lint/astcache"
	"github.com/golangci/golangci-lint/pkg/lint/linter"
	"github.com/golangci/golangci-lint/pkg/logutils"
)

type ContextLoader struct {
	cfg    *config.Config
	log    logutils.Log
	debugf logutils.DebugFunc
	goenv  *goutil.Env
}

func NewContextLoader(cfg *config.Config, log logutils.Log, goenv *goutil.Env) *ContextLoader {
	return &ContextLoader{
		cfg:    cfg,
		log:    log,
		debugf: logutils.Debug("loader"),
		goenv:  goenv,
	}
}

func (cl ContextLoader) prepareBuildContext() {
	// Set GOROOT to have working cross-compilation: cross-compiled binaries
	// have invalid GOROOT. XXX: can't use runtime.GOROOT().
	goroot := cl.goenv.Get("GOROOT")
	if goroot == "" {
		return
	}

	os.Setenv("GOROOT", goroot)
	build.Default.GOROOT = goroot
	build.Default.BuildTags = cl.cfg.Run.BuildTags
}

func (cl ContextLoader) makeFakeLoaderPackageInfo(pkg *packages.Package) *loader.PackageInfo {
	var errs []error
	for _, err := range pkg.Errors {
		errs = append(errs, err)
	}

	typeInfo := &types.Info{}
	if pkg.TypesInfo != nil {
		typeInfo = pkg.TypesInfo
	}

	return &loader.PackageInfo{
		Pkg:                   pkg.Types,
		Importable:            true, // not used
		TransitivelyErrorFree: !pkg.IllTyped,

		// use compiled (preprocessed) go files AST;
		// AST linters use not preprocessed go files AST
		Files:  pkg.Syntax,
		Errors: errs,
		Info:   *typeInfo,
	}
}

func shouldSkipPkg(pkg *packages.Package) bool {
	// it's an implicit testmain package
	return pkg.Name == "main" && strings.HasSuffix(pkg.PkgPath, ".test")
}

func (cl ContextLoader) makeFakeLoaderProgram(pkgs []*packages.Package) *loader.Program {
	var createdPkgs []*loader.PackageInfo
	for _, pkg := range pkgs {
		if len(pkg.Errors) != 0 {
			// some linters crash on packages with errors,
			// skip them and warn about them in another place
			continue
		}

		pkgInfo := cl.makeFakeLoaderPackageInfo(pkg)
		createdPkgs = append(createdPkgs, pkgInfo)
	}

	allPkgs := map[*types.Package]*loader.PackageInfo{}
	for _, pkg := range createdPkgs {
		pkg := pkg
		allPkgs[pkg.Pkg] = pkg
	}
	for _, pkg := range pkgs {
		if len(pkg.Errors) != 0 {
			// some linters crash on packages with errors,
			// skip them and warn about them in another place
			continue
		}

		for _, impPkg := range pkg.Imports {
			// don't use astcache for imported packages: we don't find issues in cgo imported deps
			pkgInfo := cl.makeFakeLoaderPackageInfo(impPkg)
			allPkgs[pkgInfo.Pkg] = pkgInfo
		}
	}

	return &loader.Program{
		Fset:        pkgs[0].Fset,
		Imported:    nil,         // not used without .Created in any linter
		Created:     createdPkgs, // all initial packages
		AllPackages: allPkgs,     // all initial packages and their depndencies
	}
}

func (cl ContextLoader) buildSSAProgram(pkgs []*packages.Package) *ssa.Program {
	startedAt := time.Now()
	defer func() {
		cl.log.Infof("SSA repr building took %s", time.Since(startedAt))
	}()

	ssaProg, _ := ssautil.Packages(pkgs, ssa.GlobalDebug)
	ssaProg.Build()
	return ssaProg
}

func (cl ContextLoader) findLoadMode(linters []linter.Config) packages.LoadMode {
	maxLoadMode := packages.LoadFiles
	for _, lc := range linters {
		curLoadMode := packages.LoadFiles
		if lc.NeedsTypeInfo {
			curLoadMode = packages.LoadSyntax
		}
		if lc.NeedsSSARepr {
			curLoadMode = packages.LoadAllSyntax
		}
		if curLoadMode > maxLoadMode {
			maxLoadMode = curLoadMode
		}
	}

	return maxLoadMode
}

func stringifyLoadMode(mode packages.LoadMode) string {
	switch mode {
	case packages.LoadFiles:
		return "load files"
	case packages.LoadImports:
		return "load imports"
	case packages.LoadTypes:
		return "load types"
	case packages.LoadSyntax:
		return "load types and syntax"
	case packages.LoadAllSyntax:
		return "load deps types and syntax"
	}
	return "unknown"
}

func (cl ContextLoader) buildArgs() []string {
	args := cl.cfg.Run.Args
	if len(args) == 0 {
		return []string{"./..."}
	}

	var retArgs []string
	for _, arg := range args {
		if strings.HasPrefix(arg, ".") {
			retArgs = append(retArgs, arg)
		} else {
			// go/packages doesn't work well if we don't have prefix ./ for local packages
			retArgs = append(retArgs, fmt.Sprintf(".%c%s", filepath.Separator, arg))
		}
	}

	return retArgs
}

func (cl ContextLoader) loadPackages(ctx context.Context, loadMode packages.LoadMode) ([]*packages.Package, error) {
	defer func(startedAt time.Time) {
		cl.log.Infof("Go packages loading at mode %s took %s", stringifyLoadMode(loadMode), time.Since(startedAt))
	}(time.Now())

	cl.prepareBuildContext()

	var buildFlags []string
	if len(cl.cfg.Run.BuildTags) != 0 {
		// go help build
		buildFlags = []string{fmt.Sprintf("-tags '%s'", strings.Join(cl.cfg.Run.BuildTags, " "))}
	}
	conf := &packages.Config{
		Mode:       loadMode,
		Tests:      cl.cfg.Run.AnalyzeTests,
		Context:    ctx,
		BuildFlags: buildFlags,
		//TODO: use fset, parsefile, overlay
	}

	args := cl.buildArgs()
	cl.debugf("Built loader args are %s", args)
	pkgs, err := packages.Load(conf, args...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load program with go/packages")
	}
	cl.debugf("loaded %d pkgs", len(pkgs))
	for i, pkg := range pkgs {
		var syntaxFiles []string
		for _, sf := range pkg.Syntax {
			syntaxFiles = append(syntaxFiles, pkg.Fset.Position(sf.Pos()).Filename)
		}
		cl.debugf("Loaded pkg #%d: ID=%s GoFiles=%s CompiledGoFiles=%s Syntax=%s",
			i, pkg.ID, pkg.GoFiles, pkg.CompiledGoFiles, syntaxFiles)
	}

	var retPkgs []*packages.Package
	for _, pkg := range pkgs {
		for _, err := range pkg.Errors {
			if strings.Contains(err.Msg, "no Go files") {
				return nil, errors.Wrapf(exitcodes.ErrNoGoFiles, "package %s", pkg.PkgPath)
			}
		}
		if !shouldSkipPkg(pkg) {
			retPkgs = append(retPkgs, pkg)
		}
	}

	return retPkgs, nil
}

func (cl ContextLoader) Load(ctx context.Context, linters []linter.Config) (*linter.Context, error) {
	loadMode := cl.findLoadMode(linters)
	pkgs, err := cl.loadPackages(ctx, loadMode)
	if err != nil {
		return nil, err
	}

	if len(pkgs) == 0 {
		return nil, exitcodes.ErrNoGoFiles
	}

	var prog *loader.Program
	if loadMode >= packages.LoadSyntax {
		prog = cl.makeFakeLoaderProgram(pkgs)
	}

	var ssaProg *ssa.Program
	if loadMode == packages.LoadAllSyntax {
		ssaProg = cl.buildSSAProgram(pkgs)
	}

	astLog := cl.log.Child("astcache")
	astCache, err := astcache.LoadFromPackages(pkgs, astLog)
	if err != nil {
		return nil, err
	}

	ret := &linter.Context{
		Packages:   pkgs,
		Program:    prog,
		SSAProgram: ssaProg,
		LoaderConfig: &loader.Config{
			Cwd:   "",  // used by depguard and fallbacked to os.Getcwd
			Build: nil, // used by depguard and megacheck and fallbacked to build.Default
		},
		Cfg:      cl.cfg,
		ASTCache: astCache,
		Log:      cl.log,
	}

	if prog != nil {
		saveNotCompilingPackages(ret)
	} else {
		for _, pkg := range pkgs {
			if len(pkg.Errors) != 0 {
				cl.log.Infof("Pkg %s errors: %v", pkg.ID, pkg.Errors)
			}
		}
	}

	return ret, nil
}

// saveNotCompilingPackages saves not compiling packages into separate slice:
// a lot of linters crash on such packages. Leave them only for those linters
// which can work with them.
func saveNotCompilingPackages(lintCtx *linter.Context) {
	for _, pkg := range lintCtx.Packages {
		if len(pkg.Errors) != 0 {
			lintCtx.NotCompilingPackages = append(lintCtx.NotCompilingPackages, pkg)
		}
	}

	if len(lintCtx.NotCompilingPackages) != 0 {
		lintCtx.Log.Infof("Packages that do not compile: %+v", lintCtx.NotCompilingPackages)
	}
}
