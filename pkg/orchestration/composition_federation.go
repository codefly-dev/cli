package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/wool"
)

type moduleRegistration struct {
	source, gatewayModule string
	target                manifest.ConsumedAPI
	secret, token         string
	expires               time.Time
}

// registrations derives claims from the same api.consumes and credentials as
// the existing host protocol. No address, port or secret is supplied by a script.
func (flow *Flow) registrations() ([]moduleRegistration, error) {
	var result []moduleRegistration
	seen := map[string]string{}
	seenSecrets := map[string]string{}
	for source, values := range flow.overrides {
		rawSecrets := values["CODEFLY__MODULE_REGISTRATION_SECRETS"]
		if values[manifest.APIConsumesEnvironmentVariable] == "" {
			continue
		}
		consumed, err := manifest.ParseConsumedAPIs(values[manifest.APIConsumesEnvironmentVariable])
		if err != nil {
			return nil, fmt.Errorf("%s: invalid consumed API projection", source)
		}
		service, err := flow.ServiceFromUnique(source)
		if err != nil {
			return nil, err
		}
		gateway := ""
		for _, dep := range service.ServiceDependencies {
			if dep.Name == "auth-gateway" {
				if gateway != "" && gateway != dep.Module {
					return nil, fmt.Errorf("%s: ambiguous host gateway", source)
				}
				gateway = dep.Module
			}
		}
		if gateway == "" {
			return nil, fmt.Errorf("%s: no declared host gateway", source)
		}
		secrets := map[string]string{}
		for _, entry := range strings.Split(rawSecrets, ",") {
			if entry == "" {
				continue
			}
			prefix, secret, ok := strings.Cut(entry, ":")
			if !ok || prefix == "" || secret == "" {
				return nil, fmt.Errorf("%s: malformed registration credentials", source)
			}
			secrets[prefix] = secret
		}
		for _, target := range consumed {
			if target.Module == gateway {
				continue
			}
			secret := secrets[target.As]
			if secret == "" {
				return nil, fmt.Errorf("%s: missing composition credential for facade %s", source, target.As)
			}
			identity := resources.ServiceUnique(target.Module, target.Service) + "/" + target.Endpoint
			key := gateway + "/" + target.As
			if prior, ok := seen[key]; ok {
				if prior != identity || seenSecrets[key] != secret {
					return nil, fmt.Errorf("facade %s has conflicting targets or credentials", key)
				}
				continue
			}
			seen[key] = identity
			seenSecrets[key] = secret
			result = append(result, moduleRegistration{source: source, gatewayModule: gateway, target: target, secret: secret})
		}
	}
	return result, nil
}

func (flow *Flow) maintainModuleFederation(ctx context.Context) {
	registrations, err := flow.registrations()
	if err != nil {
		wool.Get(ctx).Error("composition federation cannot start", wool.ErrField(err))
		return
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	interval := time.NewTicker(5 * time.Second)
	defer interval.Stop()
	lastFailure := ""
	for {
		err = flow.registerModuleRoutes(ctx, client, registrations)
		flow.federationReady.Store(err == nil)
		if err != nil && err.Error() != lastFailure && ctx.Err() == nil {
			// Only fixed errors and HTTP status are logged; response bodies and credentials are not.
			wool.Get(ctx).Warn("composition module registration pending", wool.ErrField(err))
			lastFailure = err.Error()
		} else if err == nil {
			lastFailure = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-interval.C:
		}
	}
}

func (flow *Flow) registerModuleRoutes(ctx context.Context, client *http.Client, registrations []moduleRegistration) error {
	if len(registrations) == 0 {
		return nil
	}
	configs, err := flow.ConfigurationManager.GetWorkspaceDependenciesConfigurations(ctx, "internal-auth")
	if err != nil {
		return fmt.Errorf("composition federation cannot resolve internal transport configuration")
	}
	internal := ""
	for _, config := range configs {
		for _, info := range config.Infos {
			for _, value := range info.ConfigurationValues {
				if info.Name == "internal-auth" && value.Key == "CODEFLY_INTERNAL_TOKEN" {
					internal = value.Value
				}
			}
		}
	}
	if internal == "" {
		return fmt.Errorf("composition federation has no internal transport credential")
	}
	for i := range registrations {
		registration := &registrations[i]
		gateway, err := flow.GetAddressForEndpoint(ctx, registration.gatewayModule, "auth-gateway", "rest")
		if err != nil {
			return fmt.Errorf("%s: gateway endpoint is not resolved", registration.source)
		}
		target := registration.target
		upstream, err := flow.GetAddressForEndpoint(ctx, target.Module, target.Service, target.Endpoint)
		if err != nil {
			return fmt.Errorf("%s: consumed endpoint %s/%s/%s is not resolved; include its service in this run", registration.source, target.Module, target.Service, target.Endpoint)
		}
		if err := registration.register(ctx, client, gateway, upstream, internal); err != nil {
			return fmt.Errorf("module facade %s: %w", target.As, err)
		}
	}
	return nil
}

func (r *moduleRegistration) register(ctx context.Context, client *http.Client, gateway, upstream, internal string) error {
	gateway = strings.TrimRight(gateway, "/")
	fresh := false
	if !time.Now().Add(15 * time.Second).Before(r.expires) {
		response, err := registrationPost(ctx, client, gateway+"/modules/_registration-token", map[string]string{"prefix": r.target.As}, internal, "X-Codefly-Module-Secret", r.secret)
		if err != nil {
			return err
		}
		var issued struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expiresAt"`
		}
		if json.Unmarshal(response, &issued) != nil || issued.Token == "" || !issued.ExpiresAt.After(time.Now()) {
			return fmt.Errorf("host issued an unusable registration token")
		}
		r.token, r.expires = issued.Token, issued.ExpiresAt
		fresh = true
	}
	bounded, cancel := context.WithDeadline(ctx, r.expires)
	defer cancel()
	_, err := registrationPost(bounded, client, gateway+"/modules/_register", map[string]string{"prefix": r.target.As, "upstream": upstream}, internal, "X-Codefly-Module-Registration", r.token)
	var refused registrationStatus
	if !fresh && errors.As(err, &refused) && refused == 401 {
		r.expires = time.Time{}
	}
	return err
}

type registrationStatus int

func (s registrationStatus) Error() string {
	return fmt.Sprintf("host refused registration (HTTP %d)", s)
}

func registrationPost(ctx context.Context, client *http.Client, address string, body map[string]string, internal, header, credential string) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("cannot encode registration request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid resolved registration endpoint")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Codefly-Internal-Token", internal)
	request.Header.Set(header, credential)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("registration endpoint unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, registrationStatus(response.StatusCode)
	}
	raw, err = io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return nil, fmt.Errorf("invalid registration response")
	}
	return raw, nil
}

// A local loopback route is valid only when every selected process shares the
// native host network. Check resolved preferences, not just the command flag.
func (flow *Flow) validateCompositionFederation() error {
	if len(flow.remoteServices) > 0 {
		return fmt.Errorf("composition federation does not support remote services")
	}
	for _, service := range flow.world.Dependencies.Services() {
		resolved, err := flow.ServiceFromUnique(service.Unique)
		if err != nil {
			return err
		}
		selected := flow.runtimeContextFor(resolved)
		if selected != resources.RuntimeContextNative && selected != resources.RuntimeContextNix {
			return fmt.Errorf("composition federation requires native/nix placement for %s", service.Unique)
		}
	}
	_, err := flow.registrations()
	return err
}
