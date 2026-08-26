package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"gorm.io/gorm"
)

const defaultAdministratorDisplayName = "WenzWork Administrator"

var errDefaultAdministratorCredentials = errors.New("default administrator credentials are unavailable")

type defaultAdministratorConfig struct {
	Email          string
	Password       string
	DisplayName    string
	PasswordParams auth.Argon2Params
}

type defaultAdministratorResult struct {
	Email   string
	Created bool
}

type administratorBootstrapper interface {
	SuperAdminEmail(context.Context) (string, bool, error)
	BootstrapSuperAdmin(context.Context, string, string, string, auth.Argon2Params) (auth.BootstrapResult, error)
}

type databaseAdministratorBootstrapper struct {
	db *gorm.DB
}

func (bootstrapper databaseAdministratorBootstrapper) SuperAdminEmail(ctx context.Context) (string, bool, error) {
	return auth.SuperAdminEmail(ctx, bootstrapper.db)
}

func (bootstrapper databaseAdministratorBootstrapper) BootstrapSuperAdmin(
	ctx context.Context,
	email string,
	password string,
	displayName string,
	params auth.Argon2Params,
) (auth.BootstrapResult, error) {
	return auth.BootstrapSuperAdmin(ctx, bootstrapper.db, email, password, displayName, params)
}

// ensureDefaultAdministrator makes API startup self-contained across direct,
// local-package, and portable-package launchers. Existing administrators are
// never modified, and bootstrap credentials are only required when the
// database has no super administrator.
func ensureDefaultAdministrator(
	ctx context.Context,
	bootstrapper administratorBootstrapper,
	config defaultAdministratorConfig,
) (defaultAdministratorResult, error) {
	if bootstrapper == nil {
		return defaultAdministratorResult{}, errors.New("administrator bootstrapper is required")
	}
	existingEmail, initialized, err := bootstrapper.SuperAdminEmail(ctx)
	if err != nil {
		return defaultAdministratorResult{}, fmt.Errorf("check default administrator state: %w", err)
	}
	if initialized {
		return defaultAdministratorResult{Email: existingEmail}, nil
	}

	email := strings.TrimSpace(config.Email)
	if email == "" || config.Password == "" {
		return defaultAdministratorResult{}, fmt.Errorf(
			"%w: set SYSTEM_ADMIN_EMAIL and SYSTEM_ADMIN_PASSWORD because no super administrator exists",
			errDefaultAdministratorCredentials,
		)
	}
	displayName := strings.TrimSpace(config.DisplayName)
	if displayName == "" {
		displayName = defaultAdministratorDisplayName
	}
	result, err := bootstrapper.BootstrapSuperAdmin(ctx, email, config.Password, displayName, config.PasswordParams)
	if err == nil {
		return defaultAdministratorResult{Email: result.User.Email, Created: result.Created}, nil
	}
	if !errors.Is(err, auth.ErrBootstrapAlreadyComplete) {
		return defaultAdministratorResult{}, fmt.Errorf("create default administrator: %w", err)
	}

	// Another Host may have completed the single-use bootstrap after our first
	// status read. Treat that race as success only after observing the resulting
	// super administrator; never overwrite or elevate another account.
	existingEmail, initialized, statusErr := bootstrapper.SuperAdminEmail(ctx)
	if statusErr != nil {
		return defaultAdministratorResult{}, fmt.Errorf("confirm concurrent administrator bootstrap: %w", statusErr)
	}
	if !initialized {
		return defaultAdministratorResult{}, fmt.Errorf("create default administrator: %w", err)
	}
	return defaultAdministratorResult{Email: existingEmail}, nil
}
