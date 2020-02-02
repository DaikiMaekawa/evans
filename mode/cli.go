package mode

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ktr0731/evans/config"
	"github.com/ktr0731/evans/cui"
	"github.com/ktr0731/evans/fill"
	"github.com/ktr0731/evans/idl"
	"github.com/ktr0731/evans/present"
	"github.com/ktr0731/evans/present/json"
	"github.com/ktr0731/evans/present/name"
	"github.com/ktr0731/evans/usecase"
	"github.com/ktr0731/go-multierror"
	"github.com/mattn/go-isatty"
	"github.com/pkg/errors"
)

// DefaultCLIReader is the reader that is read for inputting request values. It is exported for E2E testing.
var DefaultCLIReader io.Reader = os.Stdin

// CLIInvoker represents an invokable function for CLI mode.
type CLIInvoker func(context.Context) error

// NewCallCLIInvoker returns an CLIInvoker implementation for calling RPCs.
// If filePath is empty, the invoker tries to read input from stdin.
func NewCallCLIInvoker(ui cui.UI, rpcName, filePath string, headers config.Header) (CLIInvoker, error) {
	if rpcName == "" {
		return nil, errors.New("method is required")
	}
	// TODO: parse package and service from call.
	return func(ctx context.Context) error {
		in := DefaultCLIReader
		if filePath != "" {
			f, err := os.Open(filePath)
			if err != nil {
				return errors.Wrap(err, "failed to open the script file")
			}
			defer f.Close()
			in = f
		}
		filler := fill.NewSilentFiller(in)
		usecase.InjectPartially(usecase.Dependencies{Filler: filler})

		for k, v := range headers {
			for _, vv := range v {
				usecase.AddHeader(k, vv)
			}
		}

		err := usecase.CallRPC(ctx, ui.Writer(), rpcName)
		if err != nil {
			return errors.Wrapf(err, "failed to call RPC '%s'", rpcName)
		}
		return nil
	}, nil
}

func NewListCLIInvoker(ui cui.UI, fqn, format string) CLIInvoker {
	return func(context.Context) error {
		var presenter present.Presenter
		switch format {
		case "name", "full":
			presenter = name.NewPresenter()
		case "json":
			presenter = json.NewPresenter()
		default:
			presenter = name.NewPresenter()
		}
		usecase.InjectPartially(usecase.Dependencies{ResourcePresenter: presenter})

		pkgs := make(map[string]struct{})
		for _, p := range usecase.ListPackages() {
			pkgs[p] = struct{}{}
		}
		sp := strings.Split(fqn, ".")

		out, err := func() (string, error) {
			switch {
			case len(sp) == 1 && sp[0] == "": // Unspecified.
				var svcs []string
				for _, pkg := range usecase.ListPackages() {
					if err := usecase.UsePackage(pkg); err != nil {
						return "", errors.Wrapf(err, "failed to use package '%s'", pkg)
					}
					svc, err := usecase.FormatServices(
						&usecase.FormatServicesParams{FullyQualifiedName: format != "name"},
					)
					if err != nil {
						return "", errors.Wrap(err, "failed to list services")
					}
					svcs = append(svcs, svc)
				}
				return strings.Join(svcs, "\n"), nil

			case len(sp) == 1 && isNoPackageService(sp[0]): // No package service name.
				svc := sp[0]
				if err := usecase.UseService(svc); errors.Cause(err) == idl.ErrUnknownServiceName {
					return "", errors.Errorf("service name '%s' is not found", svc)
				} else if err != nil {
					return "", errors.Wrapf(err, "failed to use service '%s'", svc)
				}

				rpcs, err := usecase.FormatRPCs(&usecase.FormatRPCsParams{FullyQualifiedName: format != "name"})
				if err != nil {
					return "", errors.Wrap(err, "failed to format RPCs")
				}
				return rpcs, nil

			case len(sp) >= 2: // Package name and service name.
				pkg, svc := strings.Join(sp[:len(sp)-1], "."), sp[len(sp)-1]

				if err := usecase.UsePackage(pkg); errors.Cause(err) == idl.ErrUnknownPackageName {
					return "", errors.Errorf("package name '%s' is not found", pkg)
				} else if err != nil {
					return "", errors.Wrapf(err, "failed to use package '%s'", svc)
				}

				if err := usecase.UseService(svc); errors.Cause(err) == idl.ErrUnknownServiceName {
					return "", errors.Errorf("service name '%s' is not found", svc)
				} else if err != nil {
					return "", errors.Wrapf(err, "failed to use service '%s'", svc)
				}

				rpcs, err := usecase.FormatRPCs(&usecase.FormatRPCsParams{FullyQualifiedName: format != "name"})
				if err != nil {
					return "", errors.Wrap(err, "failed to format RPCs")
				}
				return rpcs, nil
			}
			return "", errors.Errorf("unknown fully-qualified package or service name '%s'", fqn)
		}()
		if err != nil {
			return err
		}
		ui.Output(out)
		return nil
	}
}

// RunAsCLIMode starts Evans as CLI mode.
func RunAsCLIMode(cfg *config.Config, invoker CLIInvoker) error {
	var injectResult error
	gRPCClient, err := newGRPCClient(cfg)
	if err != nil {
		injectResult = multierror.Append(injectResult, err)
	} else {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			gRPCClient.Close(ctx)
		}()
	}

	spec, err := newSpec(cfg, gRPCClient)
	if err != nil {
		injectResult = multierror.Append(injectResult, err)
	}

	if injectResult != nil {
		return injectResult
	}

	usecase.InjectPartially(
		usecase.Dependencies{
			Spec:              spec,
			GRPCClient:        gRPCClient,
			ResponsePresenter: json.NewPresenter(),
			ResourcePresenter: json.NewPresenter(),
		},
	)

	if err := setDefault(cfg, spec); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return invoker(ctx)
}

// IsCLIMode returns whether Evans is launched as CLI mode or not.
func IsCLIMode(file string) bool {
	return file != "" || (!isatty.IsTerminal(os.Stdin.Fd()) && !isatty.IsCygwinTerminal(os.Stdin.Fd()))
}

func isNoPackageService(n string) bool {
	// It is not need to set package because it has no package.
	svcs, err := usecase.ListServices()
	if err != nil {
		return false
	}
	for _, svc := range svcs {
		if svc == n {
			return true
		}
	}
	return false
}
