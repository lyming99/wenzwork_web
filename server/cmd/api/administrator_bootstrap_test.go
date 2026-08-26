package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wenzwork/wenzwork-web/server/internal/auth"
)

type recordingAdministratorBootstrapper struct {
	statusEmails      []string
	statusInitialized []bool
	statusErrors      []error
	statusCalls       int
	bootstrapResult   auth.BootstrapResult
	bootstrapError    error
	bootstrapCalls    int
	email             string
	password          string
	displayName       string
	passwordParams    auth.Argon2Params
}

func (bootstrapper *recordingAdministratorBootstrapper) SuperAdminEmail(context.Context) (string, bool, error) {
	index := bootstrapper.statusCalls
	bootstrapper.statusCalls++
	var email string
	var initialized bool
	var err error
	if index < len(bootstrapper.statusEmails) {
		email = bootstrapper.statusEmails[index]
	}
	if index < len(bootstrapper.statusInitialized) {
		initialized = bootstrapper.statusInitialized[index]
	}
	if index < len(bootstrapper.statusErrors) {
		err = bootstrapper.statusErrors[index]
	}
	return email, initialized, err
}

func (bootstrapper *recordingAdministratorBootstrapper) BootstrapSuperAdmin(
	_ context.Context,
	email string,
	password string,
	displayName string,
	params auth.Argon2Params,
) (auth.BootstrapResult, error) {
	bootstrapper.bootstrapCalls++
	bootstrapper.email = email
	bootstrapper.password = password
	bootstrapper.displayName = displayName
	bootstrapper.passwordParams = params
	return bootstrapper.bootstrapResult, bootstrapper.bootstrapError
}

func TestEnsureDefaultAdministratorKeepsExistingAdministratorWithoutCredentials(t *testing.T) {
	bootstrapper := &recordingAdministratorBootstrapper{
		statusEmails:      []string{"existing@example.test"},
		statusInitialized: []bool{true},
	}
	result, err := ensureDefaultAdministrator(context.Background(), bootstrapper, defaultAdministratorConfig{})
	if err != nil {
		t.Fatalf("ensureDefaultAdministrator() error = %v", err)
	}
	if result.Email != "existing@example.test" || result.Created {
		t.Fatalf("ensureDefaultAdministrator() result = %+v", result)
	}
	if bootstrapper.bootstrapCalls != 0 {
		t.Fatalf("BootstrapSuperAdmin() calls = %d, want 0", bootstrapper.bootstrapCalls)
	}
}

func TestEnsureDefaultAdministratorRequiresConfiguredCredentialsOnlyForEmptySystem(t *testing.T) {
	for _, test := range []struct {
		name     string
		email    string
		password string
	}{
		{name: "missing email", password: "administrator-password"},
		{name: "blank email", email: "  ", password: "administrator-password"},
		{name: "missing password", email: "admin@example.test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bootstrapper := &recordingAdministratorBootstrapper{}
			_, err := ensureDefaultAdministrator(context.Background(), bootstrapper, defaultAdministratorConfig{
				Email: test.email, Password: test.password,
			})
			if !errors.Is(err, errDefaultAdministratorCredentials) {
				t.Fatalf("ensureDefaultAdministrator() error = %v", err)
			}
			if bootstrapper.bootstrapCalls != 0 {
				t.Fatalf("BootstrapSuperAdmin() calls = %d, want 0", bootstrapper.bootstrapCalls)
			}
		})
	}
}

func TestEnsureDefaultAdministratorCreatesFromConfiguredDefaults(t *testing.T) {
	params := auth.Argon2Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	bootstrapper := &recordingAdministratorBootstrapper{bootstrapResult: auth.BootstrapResult{
		User: auth.User{Email: "admin@example.test"}, Created: true,
	}}
	result, err := ensureDefaultAdministrator(context.Background(), bootstrapper, defaultAdministratorConfig{
		Email: " admin@example.test ", Password: "administrator-password", PasswordParams: params,
	})
	if err != nil {
		t.Fatalf("ensureDefaultAdministrator() error = %v", err)
	}
	if result.Email != "admin@example.test" || !result.Created {
		t.Fatalf("ensureDefaultAdministrator() result = %+v", result)
	}
	if bootstrapper.bootstrapCalls != 1 || bootstrapper.email != "admin@example.test" ||
		bootstrapper.password != "administrator-password" || bootstrapper.displayName != defaultAdministratorDisplayName ||
		bootstrapper.passwordParams != params {
		t.Fatalf("BootstrapSuperAdmin() input = calls %d, email %q, password %q, display %q, params %+v",
			bootstrapper.bootstrapCalls, bootstrapper.email, bootstrapper.password, bootstrapper.displayName, bootstrapper.passwordParams)
	}
}

func TestEnsureDefaultAdministratorAcceptsConcurrentBootstrapAfterConfirmation(t *testing.T) {
	bootstrapper := &recordingAdministratorBootstrapper{
		statusEmails:      []string{"", "winner@example.test"},
		statusInitialized: []bool{false, true},
		bootstrapError:    auth.ErrBootstrapAlreadyComplete,
	}
	result, err := ensureDefaultAdministrator(context.Background(), bootstrapper, defaultAdministratorConfig{
		Email: "admin@example.test", Password: "administrator-password", DisplayName: "Configured Administrator",
	})
	if err != nil {
		t.Fatalf("ensureDefaultAdministrator() error = %v", err)
	}
	if result.Email != "winner@example.test" || result.Created || bootstrapper.statusCalls != 2 {
		t.Fatalf("ensureDefaultAdministrator() result = %+v, status calls = %d", result, bootstrapper.statusCalls)
	}
}

func TestEnsureDefaultAdministratorPropagatesStateAndCreationFailuresWithoutSecrets(t *testing.T) {
	stateFailure := errors.New("database unavailable")
	_, err := ensureDefaultAdministrator(context.Background(), &recordingAdministratorBootstrapper{
		statusErrors: []error{stateFailure},
	}, defaultAdministratorConfig{})
	if !errors.Is(err, stateFailure) {
		t.Fatalf("state failure = %v", err)
	}

	creationFailure := errors.New("insert rejected")
	secret := "never-print-this-password"
	_, err = ensureDefaultAdministrator(context.Background(), &recordingAdministratorBootstrapper{
		bootstrapError: creationFailure,
	}, defaultAdministratorConfig{Email: "admin@example.test", Password: secret})
	if !errors.Is(err, creationFailure) || strings.Contains(err.Error(), secret) {
		t.Fatalf("creation failure = %v", err)
	}
}
