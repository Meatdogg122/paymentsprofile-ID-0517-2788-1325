package codespaces

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/cli/cli/v2/internal/codespaces/api"
	"github.com/cli/cli/v2/pkg/cmd/codespace/output"
	"github.com/cli/cli/v2/pkg/liveshare"
	"github.com/muhammadmuzzammil1998/jsonc"
	"golang.org/x/sync/errgroup"
)

// ListPorts lists known ports in a codespace.
func (a *App) ListPorts(ctx context.Context, codespaceName string, asJSON bool) (err error) {
	codespace, err := a.getOrChooseCodespace(ctx, codespaceName)
	if err != nil {
		// TODO(josebalius): remove special handling of this error here and it other places
		if err == errNoCodespaces {
			return err
		}
		return fmt.Errorf("error choosing codespace: %w", err)
	}

	devContainerCh := getDevContainer(ctx, a.apiClient, codespace)

	session, err := a.connectToLiveshare(ctx, noopLogger(), codespace)
	if err != nil {
		return fmt.Errorf("error connecting to Live Share: %w", err)
	}
	defer safeClose(session, &err)

	a.logger.Println("Loading ports...")
	ports, err := session.GetSharedServers(ctx)
	if err != nil {
		return fmt.Errorf("error getting ports of shared servers: %w", err)
	}

	devContainerResult := <-devContainerCh
	if devContainerResult.err != nil {
		// Warn about failure to read the devcontainer file. Not a codespace command error.
		a.errLogger.Printf("Failed to get port names: %v\n", devContainerResult.err.Error())
	}

	table := output.NewTable(os.Stdout, asJSON)
	table.SetHeader([]string{"Label", "Port", "Privacy", "Browse URL"})
	for _, port := range ports {
		sourcePort := strconv.Itoa(port.SourcePort)
		var portName string
		if devContainerResult.devContainer != nil {
			if attributes, ok := devContainerResult.devContainer.PortAttributes[sourcePort]; ok {
				portName = attributes.Label
			}
		}

		table.Append([]string{
			portName,
			sourcePort,
			port.Privacy,
			fmt.Sprintf("https://%s-%s.githubpreview.dev/", codespace.Name, sourcePort),
		})
	}
	table.Render()

	return nil
}

type devContainerResult struct {
	devContainer *devContainer
	err          error
}

type devContainer struct {
	PortAttributes map[string]portAttribute `json:"portsAttributes"`
}

type portAttribute struct {
	Label string `json:"label"`
}

func getDevContainer(ctx context.Context, apiClient apiClient, codespace *api.Codespace) <-chan devContainerResult {
	ch := make(chan devContainerResult, 1)
	go func() {
		contents, err := apiClient.GetCodespaceRepositoryContents(ctx, codespace, ".devcontainer/devcontainer.json")
		if err != nil {
			ch <- devContainerResult{nil, fmt.Errorf("error getting content: %w", err)}
			return
		}

		if contents == nil {
			ch <- devContainerResult{nil, nil}
			return
		}

		convertedJSON := normalizeJSON(jsonc.ToJSON(contents))
		if !jsonc.Valid(convertedJSON) {
			ch <- devContainerResult{nil, errors.New("failed to convert json to standard json")}
			return
		}

		var container devContainer
		if err := json.Unmarshal(convertedJSON, &container); err != nil {
			ch <- devContainerResult{nil, fmt.Errorf("error unmarshaling: %w", err)}
			return
		}

		ch <- devContainerResult{&container, nil}
	}()
	return ch
}

func normalizeJSON(j []byte) []byte {
	// remove trailing commas
	return bytes.ReplaceAll(j, []byte("},}"), []byte("}}"))
}

func (a *App) UpdatePortPrivacy(ctx context.Context, codespaceName string, args []string) (err error) {
	ports, err := a.parsePortPrivacies(args)
	if err != nil {
		return fmt.Errorf("error parsing port arguments: %w", err)
	}
	codespace, err := a.getOrChooseCodespace(ctx, codespaceName)
	if err != nil {
		if err == errNoCodespaces {
			return err
		}
		return fmt.Errorf("error getting codespace: %w", err)
	}

	session, err := a.connectToLiveshare(ctx, noopLogger(), codespace)
	if err != nil {
		return fmt.Errorf("error connecting to Live Share: %w", err)
	}
	defer safeClose(session, &err)

	for _, port := range ports {
		if err := session.UpdateSharedServerPrivacy(ctx, port.number, port.privacy); err != nil {
			return fmt.Errorf("error update port to public: %w", err)
		}

		a.logger.Printf("Port %d is now %s scoped.\n", port.number, port.privacy)
	}

	return nil
}

type portPrivacy struct {
	number  int
	privacy string
}

func (a *App) parsePortPrivacies(args []string) ([]portPrivacy, error) {
	ports := make([]portPrivacy, 0, len(args))
	for _, a := range args {
		fields := strings.Split(a, ":")
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid port privacy format for %q", a)
		}
		portStr, privacy := fields[0], fields[1]
		portNumber, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid port number: %w", err)
		}
		ports = append(ports, portPrivacy{portNumber, privacy})
	}
	return ports, nil
}

func (a *App) ForwardPorts(ctx context.Context, codespaceName string, ports []string) (err error) {
	portPairs, err := getPortPairs(ports)
	if err != nil {
		return fmt.Errorf("get port pairs: %w", err)
	}

	codespace, err := a.getOrChooseCodespace(ctx, codespaceName)
	if err != nil {
		if err == errNoCodespaces {
			return err
		}
		return fmt.Errorf("error getting codespace: %w", err)
	}

	session, err := a.connectToLiveshare(ctx, noopLogger(), codespace)
	if err != nil {
		return fmt.Errorf("error connecting to Live Share: %w", err)
	}
	defer safeClose(session, &err)

	// Run forwarding of all ports concurrently, aborting all of
	// them at the first failure, including cancellation of the context.
	group, ctx := errgroup.WithContext(ctx)
	for _, pair := range portPairs {
		pair := pair
		group.Go(func() error {
			listen, err := net.Listen("tcp", fmt.Sprintf(":%d", pair.local))
			if err != nil {
				return err
			}
			defer listen.Close()
			a.logger.Printf("Forwarding ports: remote %d <=> local %d\n", pair.remote, pair.local)
			name := fmt.Sprintf("share-%d", pair.remote)
			fwd := liveshare.NewPortForwarder(session, name, pair.remote, false)
			return fwd.ForwardToListener(ctx, listen) // error always non-nil
		})
	}
	return group.Wait() // first error
}

type portPair struct {
	remote, local int
}

// getPortPairs parses a list of strings of form "%d:%d" into pairs of (remote, local) numbers.
func getPortPairs(ports []string) ([]portPair, error) {
	pp := make([]portPair, 0, len(ports))

	for _, portString := range ports {
		parts := strings.Split(portString, ":")
		if len(parts) < 2 {
			return nil, fmt.Errorf("port pair: %q is not valid", portString)
		}

		remote, err := strconv.Atoi(parts[0])
		if err != nil {
			return pp, fmt.Errorf("convert remote port to int: %w", err)
		}

		local, err := strconv.Atoi(parts[1])
		if err != nil {
			return pp, fmt.Errorf("convert local port to int: %w", err)
		}

		pp = append(pp, portPair{remote, local})
	}

	return pp, nil
}