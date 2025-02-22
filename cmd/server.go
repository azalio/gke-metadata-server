// MIT License
//
// Copyright (c) 2023 Matheus Pimenta
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package main

import (
	"fmt"
	"net/netip"
	"os"
	"time"

	"github.com/matheuscscp/gke-metadata-server/internal/googlecredentials"
	"github.com/matheuscscp/gke-metadata-server/internal/logging"
	"github.com/matheuscscp/gke-metadata-server/internal/metrics"
	getnode "github.com/matheuscscp/gke-metadata-server/internal/node/get"
	watchnode "github.com/matheuscscp/gke-metadata-server/internal/node/watch"
	listpods "github.com/matheuscscp/gke-metadata-server/internal/pods/list"
	watchpods "github.com/matheuscscp/gke-metadata-server/internal/pods/watch"
	"github.com/matheuscscp/gke-metadata-server/internal/redirect"
	"github.com/matheuscscp/gke-metadata-server/internal/server"
	"github.com/matheuscscp/gke-metadata-server/internal/serviceaccounts"
	getserviceaccount "github.com/matheuscscp/gke-metadata-server/internal/serviceaccounts/get"
	watchserviceaccounts "github.com/matheuscscp/gke-metadata-server/internal/serviceaccounts/watch"
	cacheserviceaccounttokens "github.com/matheuscscp/gke-metadata-server/internal/serviceaccounttokens/cache"
	createserviceaccounttoken "github.com/matheuscscp/gke-metadata-server/internal/serviceaccounttokens/create"

	"github.com/spf13/cobra"
)

func newServerCommand() *cobra.Command {
	var (
		serverPort                          int
		workloadIdentityProvider            string
		nodePoolServiceAccountName          string
		nodePoolServiceAccountNamespace     string
		watchPods                           bool
		watchPodsResyncPeriod               time.Duration
		watchPodsDisableFallback            bool
		watchNode                           bool
		watchNodeResyncPeriod               time.Duration
		watchNodeDisableFallback            bool
		watchServiceAccounts                bool
		watchServiceAccountsResyncPeriod    time.Duration
		watchServiceAccountsDisableFallback bool
		cacheTokens                         bool
		cacheTokensConcurrency              int
	)

	cmd := &cobra.Command{
		Use:   "server",
		Short: "Start the GKE Metadata Server emulator",
		Long:  "Start the GKE Metadata Server emulator for serving requests locally inside each node of the Kubernes cluster",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			// validate input
			nodeName := os.Getenv("NODE_NAME")
			if nodeName == "" {
				return fmt.Errorf("NODE_NAME environment variable must be specified")
			}
			podIP := os.Getenv("POD_IP")
			if podIP == "" {
				return fmt.Errorf("POD_IP environment variable must be specified")
			}
			emulatorIP, err := netip.ParseAddr(podIP)
			if err != nil {
				return fmt.Errorf("error parsing POD_IP environment variable: %w", err)
			}
			if !emulatorIP.Is4() {
				return fmt.Errorf("POD_IP environment variable must be an IPv4 address")
			}
			nodePoolSANameSet := nodePoolServiceAccountName != ""
			nodePoolSANamespaceSet := nodePoolServiceAccountNamespace != ""
			if nodePoolSANameSet != nodePoolSANamespaceSet {
				return fmt.Errorf("--node-pool-service-account-name and --node-pool-service-account-namespace arguments must be either both specified or both empty")
			}
			var nodePoolServiceAccount *serviceaccounts.Reference
			if nodePoolSANameSet {
				nodePoolServiceAccount = &serviceaccounts.Reference{
					Name:      nodePoolServiceAccountName,
					Namespace: nodePoolServiceAccountNamespace,
				}
			}
			googleCredentialsConfig, workloadIdentityPool, err := googlecredentials.NewConfig(googlecredentials.ConfigOptions{
				WorkloadIdentityProvider: workloadIdentityProvider,
			})
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			l := logging.FromContext(ctx)

			// now there are only runtime errors below
			defer func() {
				if runtimeErr := err; err != nil {
					err = nil
					l.WithError(runtimeErr).Fatal("runtime error")
				}
			}()

			// install ebpf redirect program
			redirBPF, err := redirect.LoadAndAttachBPF(emulatorIP, serverPort, logging.Debug())
			if err != nil {
				return fmt.Errorf("error loading eBPF redirect program: %w", err)
			}
			defer redirBPF.Close()

			// create clients
			kubeClient, err := createKubernetesClient(ctx)
			if err != nil {
				return fmt.Errorf("error creating k8s client: %w", err)
			}

			metricsRegistry := metrics.NewRegistry()

			// create pod provider
			pods := listpods.NewProvider(listpods.ProviderOptions{
				NodeName:   nodeName,
				KubeClient: kubeClient,
			})
			var wp *watchpods.Provider
			if watchPods {
				opts := watchpods.ProviderOptions{
					FallbackSource:  pods,
					NodeName:        nodeName,
					KubeClient:      kubeClient,
					MetricsRegistry: metricsRegistry,
					ResyncPeriod:    watchPodsResyncPeriod,
				}
				if watchPodsDisableFallback {
					opts.FallbackSource = nil
				}
				wp = watchpods.NewProvider(opts)
				defer wp.Close()
				pods = wp
			}

			// create node provider
			node := getnode.NewProvider(getnode.ProviderOptions{
				NodeName:   nodeName,
				KubeClient: kubeClient,
			})
			var wn *watchnode.Provider
			if watchNode {
				opts := watchnode.ProviderOptions{
					FallbackSource: node,
					NodeName:       nodeName,
					KubeClient:     kubeClient,
					ResyncPeriod:   watchNodeResyncPeriod,
				}
				if watchNodeDisableFallback {
					opts.FallbackSource = nil
				}
				wn = watchnode.NewProvider(opts)
				defer wn.Close()
				node = wn
			}

			// create service account provider
			serviceAccounts := getserviceaccount.NewProvider(getserviceaccount.ProviderOptions{
				KubeClient: kubeClient,
			})
			var wsa *watchserviceaccounts.Provider
			if watchServiceAccounts {
				opts := watchserviceaccounts.ProviderOptions{
					FallbackSource:  serviceAccounts,
					KubeClient:      kubeClient,
					MetricsRegistry: metricsRegistry,
					ResyncPeriod:    watchServiceAccountsResyncPeriod,
				}
				if watchServiceAccountsDisableFallback {
					opts.FallbackSource = nil
				}
				wsa = watchserviceaccounts.NewProvider(ctx, opts)
				defer wsa.Close()
				serviceAccounts = wsa
			}

			// create service account token provider
			serviceAccountTokens := createserviceaccounttoken.NewProvider(createserviceaccounttoken.ProviderOptions{
				GoogleCredentialsConfig: googleCredentialsConfig,
				KubeClient:              kubeClient,
			})
			if cacheTokens {
				p := cacheserviceaccounttokens.NewProvider(ctx, cacheserviceaccounttokens.ProviderOptions{
					Source:                 serviceAccountTokens,
					ServiceAccounts:        serviceAccounts,
					MetricsRegistry:        metricsRegistry,
					Concurrency:            cacheTokensConcurrency,
					NodePoolServiceAccount: nodePoolServiceAccount,
				})
				defer p.Close()
				if wp != nil {
					wp.AddListener(p)
				}
				if wsa != nil {
					wsa.AddListener(p)
				}
				serviceAccountTokens = p
			}

			// start server
			if wp != nil {
				wp.Start(ctx)
			}
			if wn != nil {
				wn.Start(ctx)
			}
			if wsa != nil {
				wsa.Start(ctx)
			}
			s := server.New(ctx, server.ServerOptions{
				NodeName:               nodeName,
				ServerPort:             serverPort,
				Pods:                   pods,
				Node:                   node,
				ServiceAccounts:        serviceAccounts,
				ServiceAccountTokens:   serviceAccountTokens,
				MetricsRegistry:        metricsRegistry,
				NodePoolServiceAccount: nodePoolServiceAccount,
				WorkloadIdentityPool:   workloadIdentityPool,
			})

			ctx, cancel := waitForShutdown(ctx)
			defer cancel()
			if err := s.Shutdown(ctx); err != nil {
				return fmt.Errorf("error in server graceful shutdown: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().IntVar(&serverPort, "server-port", 8080,
		"Network address where the metadata server must listen on")
	cmd.Flags().StringVar(&workloadIdentityProvider, "workload-identity-provider", "",
		"Mandatory fully-qualified resource name of the GCP Workload Identity Provider (projects/<project_number>/locations/global/workloadIdentityPools/<pool_name>/providers/<provider_name>)")
	cmd.Flags().StringVar(&nodePoolServiceAccountName, "node-pool-service-account-name", "",
		"Name of the default service account to be used by pods running on the host network")
	cmd.Flags().StringVar(&nodePoolServiceAccountNamespace, "node-pool-service-account-namespace", "",
		"Namespace of the default service account to be used by pods running on the host network")
	cmd.Flags().BoolVar(&watchPods, "watch-pods", false,
		"Whether or not to watch the pods running on the same node (default false)")
	cmd.Flags().DurationVar(&watchPodsResyncPeriod, "watch-pods-resync-period", 10*time.Minute,
		"When watching the pods running on the same node, how often to fully resync")
	cmd.Flags().BoolVar(&watchPodsDisableFallback, "watch-pods-disable-fallback", false,
		"When watching the pods running on the same node, whether or not to disable the use of a simple fallback method for retrieving pods upon cache misses (default false)")
	cmd.Flags().BoolVar(&watchNode, "watch-node", false,
		"Whether or not to watch the node where the metadata server is running (default false)")
	cmd.Flags().DurationVar(&watchNodeResyncPeriod, "watch-node-resync-period", time.Hour,
		"When watching the node where the metadata server is running, how often to fully resync")
	cmd.Flags().BoolVar(&watchNodeDisableFallback, "watch-node-disable-fallback", false,
		"When watching the node where the metadata server is running, whether or not to disable the use of a simple fallback method for retrieving the node upon cache misses (default false)")
	cmd.Flags().BoolVar(&watchServiceAccounts, "watch-service-accounts", false,
		"Whether or not to watch all the service accounts of the cluster (default false)")
	cmd.Flags().DurationVar(&watchServiceAccountsResyncPeriod, "watch-service-accounts-resync-period", time.Hour,
		"When watching service accounts, how often to fully resync")
	cmd.Flags().BoolVar(&watchServiceAccountsDisableFallback, "watch-service-accounts-disable-fallback", false,
		"When watching service accounts, whether or not to disable the use of a simple fallback method for retrieving service accounts upon cache misses (default false)")
	cmd.Flags().BoolVar(&cacheTokens, "cache-tokens", false,
		"Whether or not to proactively cache tokens for the service accounts used by the pods running on the same node (default false)")
	cmd.Flags().IntVar(&cacheTokensConcurrency, "cache-tokens-concurrency", 10,
		"When proactively caching service account tokens, what is the maximum amount of caching operations that can happen in parallel")

	return cmd
}
