// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cert

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-multierror"
	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/helper/ocsp"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const operationPrefixCert = "cert"

func Factory(ctx context.Context, conf *logical.BackendConfig) (logical.Backend, error) {
	b := Backend()
	if err := b.Setup(ctx, conf); err != nil {
		return nil, err
	}
	return b, nil
}

func Backend() *backend {
	var b backend
	b.Backend = &framework.Backend{
		Help: backendHelp,
		PathsSpecial: &logical.Paths{
			Unauthenticated: []string{
				"login",
			},
		},
		Paths: []*framework.Path{
			pathConfig(&b),
			pathLogin(&b),
			pathListCerts(&b),
			pathCerts(&b),
			pathListCRLs(&b),
			pathCRLs(&b),
		},
		AuthRenew:      b.loginPathWrapper(b.pathLoginRenew),
		Invalidate:     b.invalidate,
		BackendType:    logical.TypeCredential,
		InitializeFunc: b.initialize,
		PeriodicFunc:   b.updateCRLs,
	}

	b.crlUpdateMutex = &sync.RWMutex{}
	return &b
}

type backend struct {
	*framework.Backend
	MapCertId *framework.PathMap

	crls            map[string]CRLInfo
	crlUpdateMutex  *sync.RWMutex
	ocspClientMutex sync.RWMutex
	ocspClient      *ocsp.Client
	configUpdated   atomic.Bool
}

func (b *backend) initialize(ctx context.Context, req *logical.InitializationRequest) error {
	bConf, err := b.Config(ctx, req.Storage)
	if err != nil {
		b.Logger().Error(fmt.Sprintf("failed to load backend configuration: %v", err))
		return err
	}

	if bConf != nil {
		b.updatedConfig(bConf)
	}

	if err := b.lockThenpopulateCRLs(ctx, req.Storage); err != nil {
		b.Logger().Error(fmt.Sprintf("failed to populate CRLs: %v", err))
		return err
	}

	return nil
}

func (b *backend) invalidate(_ context.Context, key string) {
	switch {
	case strings.HasPrefix(key, "crls/"):
		b.crlUpdateMutex.Lock()
		defer b.crlUpdateMutex.Unlock()
		b.crls = nil
	case key == "config":
		b.configUpdated.Store(true)
	}
}

func (b *backend) initOCSPClient(cacheSize int) {
	b.ocspClient = ocsp.New(func() hclog.Logger {
		return b.Logger()
	}, cacheSize)
}

func (b *backend) updatedConfig(config *config) {
	b.ocspClientMutex.Lock()
	defer b.ocspClientMutex.Unlock()
	b.initOCSPClient(config.OcspCacheSize)
	b.configUpdated.Store(false)
}

func (b *backend) fetchCRL(ctx context.Context, storage logical.Storage, name string, crl *CRLInfo) error {
	response, err := http.Get(crl.CDP.Url)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusOK {
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		certList, err := x509.ParseCRL(body)
		if err != nil {
			return err
		}
		crl.CDP.ValidUntil = certList.TBSCertList.NextUpdate
		return b.setCRL(ctx, storage, certList, name, crl.CDP)
	}
	return fmt.Errorf("unexpected response code %d fetching CRL from %s", response.StatusCode, crl.CDP.Url)
}

func (b *backend) updateCRLs(ctx context.Context, req *logical.Request) error {
	b.crlUpdateMutex.Lock()
	defer b.crlUpdateMutex.Unlock()
	var errs *multierror.Error
	for name, crl := range b.crls {
		if crl.CDP != nil && time.Now().After(crl.CDP.ValidUntil) {
			if err := b.fetchCRL(ctx, req.Storage, name, &crl); err != nil {
				errs = multierror.Append(errs, err)
			}
		}
	}
	return errs.ErrorOrNil()
}

func (b *backend) storeConfig(ctx context.Context, storage logical.Storage, config *config) error {
	entry, err := logical.StorageEntryJSON("config", config)
	if err != nil {
		return err
	}

	if err := storage.Put(ctx, entry); err != nil {
		return err
	}
	b.updatedConfig(config)
	return nil
}

const backendHelp = `
The "cert" credential provider allows authentication using
TLS client certificates. A client connects to OpenBao and uses
the "login" endpoint to generate a client token.

Trusted certificates are configured using the "certs/" endpoint
by a user with root access. A certificate authority can be trusted,
which permits all keys signed by it. Alternatively, self-signed
certificates can be trusted avoiding the need for a CA.
`
