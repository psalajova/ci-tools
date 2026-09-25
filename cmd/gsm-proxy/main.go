package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/option"

	"sigs.k8s.io/prow/pkg/interrupts"
	"sigs.k8s.io/prow/pkg/logrusutil"

	gsmproxy "github.com/openshift/ci-tools/pkg/gsm-proxy"
)

type options struct {
	listenAddr                string
	audience                  string
	project                   string
	syncRoverGroupsConfigPath string
	groupsKerberosIdsPath     string
	credentialsFile           string
	gracePeriod               time.Duration
}

func gatherOptions() (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.StringVar(&o.listenAddr, "listen-addr", ":8080", "The address the proxy listens on")
	fs.StringVar(&o.audience, "audience", gsmproxy.GcloudOAuthClientID, "The 'aud' claim identity tokens must carry. The default is gcloud's own client ID, which is what 'gcloud auth print-identity-token' stamps; override only when the CLI mints its token some other way. Must not be empty.")
	fs.StringVar(&o.project, "gcp-project", "openshift-ci-secrets", "The GCP project holding the secrets")
	fs.StringVar(&o.syncRoverGroupsConfigPath, "config-file", "", "Path to core-services/sync-rover-groups/_config.yaml, which says which group owns which collection. Must not be empty.")
	fs.StringVar(&o.groupsKerberosIdsPath, "groups-file", "", "Path to groups.yaml, which maps a rover group to its kerberos ids. Must not be empty.")
	fs.StringVar(&o.credentialsFile, "gcp-service-account-credentials-file", "", "Path to a GCP service account credentials file. Must not be empty.")
	fs.DurationVar(&o.gracePeriod, "grace-period", 10*time.Second, "Grace period for server shutdown")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, fmt.Errorf("failed to parse flags: %w", err)
	}
	return o, nil
}

func (o *options) validate() error {
	if o.audience == "" {
		return errors.New("--audience must not be empty")
	}
	if o.syncRoverGroupsConfigPath == "" {
		return errors.New("--config-file must not be empty")
	}
	if o.groupsKerberosIdsPath == "" {
		return errors.New("--groups-file must not be empty")
	}
	if o.credentialsFile == "" {
		return errors.New("--gcp-service-account-credentials-file must not be empty")
	}
	if o.project == "" {
		return errors.New("--gcp-project must not be empty")
	}
	return nil
}

func main() {
	logrusutil.ComponentInit()
	o, err := gatherOptions()
	if err != nil {
		logrus.WithError(err).Fatal("Failed to gather options")
	}
	if err := o.validate(); err != nil {
		logrus.WithError(err).Fatal("Invalid options")
	}

	ctx := interrupts.Context()

	// The handler re-reads these two files on every request; load them once here so a bad mount or
	// a missing ConfigMap fails the deployment at startup instead of on the first user's request.
	if _, err := gsmproxy.LoadAuthorizer(o.syncRoverGroupsConfigPath, o.groupsKerberosIdsPath); err != nil {
		logrus.WithError(err).Fatal("Failed to load authorization data")
	}

	smClient, err := secretmanager.NewClient(ctx, option.WithCredentialsFile(o.credentialsFile))
	if err != nil {
		logrus.WithError(err).Fatal("Failed to create the Secret Manager client")
	}
	defer func() {
		if err := smClient.Close(); err != nil {
			logrus.WithError(err).Warn("Failed to close the Secret Manager client")
		}
	}()

	handler := &gsmproxy.ListSecretsHandler{
		Verifier: &gsmproxy.GoogleIdentityVerifier{Audience: o.audience, Validator: gsmproxy.GoogleTokenValidator{}},
		Lister:   &gsmproxy.GSMLister{Client: smClient, Project: o.project},
		Authorizer: func() (*gsmproxy.Authorizer, error) {
			return gsmproxy.LoadAuthorizer(o.syncRoverGroupsConfigPath, o.groupsKerberosIdsPath)
		},
		Log: logrus.WithField("component", "list-secrets"),
	}

	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/collections/{collection}/secrets", handler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:              o.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	logrus.WithFields(logrus.Fields{"listen-addr": o.listenAddr, "audience": o.audience}).Info("Starting gsm-proxy")
	interrupts.ListenAndServe(server, o.gracePeriod)
	interrupts.WaitForGracefulShutdown()
}
