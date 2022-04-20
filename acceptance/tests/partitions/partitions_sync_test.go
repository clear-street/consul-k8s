package partitions

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	terratestk8s "github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/hashicorp/consul-k8s/acceptance/framework/consul"
	"github.com/hashicorp/consul-k8s/acceptance/framework/environment"
	"github.com/hashicorp/consul-k8s/acceptance/framework/helpers"
	"github.com/hashicorp/consul-k8s/acceptance/framework/k8s"
	"github.com/hashicorp/consul-k8s/acceptance/framework/logger"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/sdk/testutil/retry"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Test that Connect works in a default and ACLsAndAutoEncryptEnabled installations for X-Partition and in-partition networking.
func TestPartitions_Sync(t *testing.T) {
	env := suite.Environment()
	cfg := suite.Config()

	if !cfg.EnableEnterprise {
		t.Skipf("skipping this test because -enable-enterprise is not set")
	}

	const defaultPartition = "default"
	const secondaryPartition = "secondary"
	const defaultNamespace = "default"
	cases := []struct {
		name                      string
		destinationNamespace      string
		mirrorK8S                 bool
		ACLsAndAutoEncryptEnabled bool
	}{
		{
			"default destination namespace",
			defaultNamespace,
			false,
			false,
		},
		{
			"default destination namespace; ACLs and auto-encrypt enabled",
			defaultNamespace,
			false,
			true,
		},
		{
			"single destination namespace",
			staticServerNamespace,
			false,
			false,
		},
		{
			"single destination namespace; ACLs and auto-encrypt enabled",
			staticServerNamespace,
			false,
			true,
		},
		{
			"mirror k8s namespaces",
			staticServerNamespace,
			true,
			false,
		},
		{
			"mirror k8s namespaces; ACLs and auto-encrypt enabled",
			staticServerNamespace,
			true,
			true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			serverClusterContext := env.DefaultContext(t)
			clientClusterContext := env.Context(t, environment.SecondaryContextName)

			ctx := context.Background()

			commonHelmValues := map[string]string{
				"global.adminPartitions.enabled": "true",

				"global.enableConsulNamespaces": "true",

				"global.tls.enabled":           "true",
				"global.tls.httpsOnly":         strconv.FormatBool(c.ACLsAndAutoEncryptEnabled),
				"global.tls.enableAutoEncrypt": strconv.FormatBool(c.ACLsAndAutoEncryptEnabled),

				"global.acls.manageSystemACLs": strconv.FormatBool(c.ACLsAndAutoEncryptEnabled),

				"syncCatalog.enabled": "true",
				// When mirroringK8S is set, this setting is ignored.
				"syncCatalog.consulNamespaces.consulDestinationNamespace": c.destinationNamespace,
				"syncCatalog.consulNamespaces.mirroringK8S":               strconv.FormatBool(c.mirrorK8S),
				"syncCatalog.addK8SNamespaceSuffix":                       "false",

				"controller.enabled": "true",

				"dns.enabled":           "true",
				"dns.enableRedirection": strconv.FormatBool(cfg.EnableTransparentProxy),
			}

			serverHelmValues := map[string]string{
				"server.exposeGossipAndRPCPorts": "true",
			}

			// On Kind, there are no load balancers but since all clusters
			// share the same node network (docker bridge), we can use
			// a NodePort service so that we can access node(s) in a different Kind cluster.
			if cfg.UseKind {
				serverHelmValues["global.adminPartitions.service.type"] = "NodePort"
				serverHelmValues["global.adminPartitions.service.nodePort.https"] = "30000"
			}

			releaseName := helpers.RandomName()

			helpers.MergeMaps(serverHelmValues, commonHelmValues)

			// Install the consul cluster with servers in the default kubernetes context.
			serverConsulCluster := consul.NewHelmCluster(t, serverHelmValues, serverClusterContext, cfg, releaseName)
			serverConsulCluster.Create(t)

			// Get the TLS CA certificate and key secret from the server cluster and apply it to the client cluster.
			caCertSecretName := fmt.Sprintf("%s-consul-ca-cert", releaseName)
			caKeySecretName := fmt.Sprintf("%s-consul-ca-key", releaseName)

			logger.Logf(t, "retrieving ca cert secret %s from the server cluster and applying to the client cluster", caCertSecretName)
			moveSecret(t, serverClusterContext, clientClusterContext, caCertSecretName)

			if !c.ACLsAndAutoEncryptEnabled {
				// When auto-encrypt is disabled, we need both
				// the CA cert and CA key to be available in the clients cluster to generate client certificates and keys.
				logger.Logf(t, "retrieving ca key secret %s from the server cluster and applying to the client cluster", caKeySecretName)
				moveSecret(t, serverClusterContext, clientClusterContext, caKeySecretName)
			}

			partitionToken := fmt.Sprintf("%s-consul-partitions-acl-token", releaseName)
			if c.ACLsAndAutoEncryptEnabled {
				logger.Logf(t, "retrieving partition token secret %s from the server cluster and applying to the client cluster", partitionToken)
				moveSecret(t, serverClusterContext, clientClusterContext, partitionToken)
			}

			partitionServiceName := fmt.Sprintf("%s-consul-partition", releaseName)
			partitionSvcAddress := k8s.ServiceHost(t, cfg, serverClusterContext, partitionServiceName)

			k8sAuthMethodHost := k8s.KubernetesAPIServerHost(t, cfg, clientClusterContext)

			// Create client cluster.
			clientHelmValues := map[string]string{
				"global.enabled": "false",

				"global.adminPartitions.name": secondaryPartition,

				"global.tls.caCert.secretName": caCertSecretName,
				"global.tls.caCert.secretKey":  "tls.crt",

				"externalServers.enabled":       "true",
				"externalServers.hosts[0]":      partitionSvcAddress,
				"externalServers.tlsServerName": "server.dc1.consul",

				"client.enabled":           "true",
				"client.exposeGossipPorts": "true",
				"client.join[0]":           partitionSvcAddress,
			}

			if c.ACLsAndAutoEncryptEnabled {
				// Setup partition token and auth method host if ACLs enabled.
				clientHelmValues["global.acls.bootstrapToken.secretName"] = partitionToken
				clientHelmValues["global.acls.bootstrapToken.secretKey"] = "token"
				clientHelmValues["externalServers.k8sAuthMethodHost"] = k8sAuthMethodHost
			} else {
				// Provide CA key when auto-encrypt is disabled.
				clientHelmValues["global.tls.caKey.secretName"] = caKeySecretName
				clientHelmValues["global.tls.caKey.secretKey"] = "tls.key"
			}

			if cfg.UseKind {
				clientHelmValues["externalServers.httpsPort"] = "30000"
			}

			helpers.MergeMaps(clientHelmValues, commonHelmValues)

			// Install the consul cluster without servers in the client cluster kubernetes context.
			clientConsulCluster := consul.NewHelmCluster(t, clientHelmValues, clientClusterContext, cfg, releaseName)
			clientConsulCluster.Create(t)

			// Ensure consul clients are created.
			agentPodList, err := clientClusterContext.KubernetesClient(t).CoreV1().Pods(clientClusterContext.KubectlOptions(t).Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=consul,component=client"})
			require.NoError(t, err)
			require.NotEmpty(t, agentPodList.Items)

			output, err := k8s.RunKubectlAndGetOutputE(t, clientClusterContext.KubectlOptions(t), "logs", agentPodList.Items[0].Name, "-n", clientClusterContext.KubectlOptions(t).Namespace)
			require.NoError(t, err)
			require.Contains(t, output, "Partition: 'secondary'")

			serverClusterStaticServerOpts := &terratestk8s.KubectlOptions{
				ContextName: serverClusterContext.KubectlOptions(t).ContextName,
				ConfigPath:  serverClusterContext.KubectlOptions(t).ConfigPath,
				Namespace:   staticServerNamespace,
			}
			clientClusterStaticServerOpts := &terratestk8s.KubectlOptions{
				ContextName: clientClusterContext.KubectlOptions(t).ContextName,
				ConfigPath:  clientClusterContext.KubectlOptions(t).ConfigPath,
				Namespace:   staticServerNamespace,
			}

			logger.Logf(t, "creating namespaces %s in servers cluster", staticServerNamespace)
			k8s.RunKubectl(t, serverClusterContext.KubectlOptions(t), "create", "ns", staticServerNamespace)
			helpers.Cleanup(t, cfg.NoCleanupOnFailure, func() {
				k8s.RunKubectl(t, serverClusterContext.KubectlOptions(t), "delete", "ns", staticServerNamespace)
			})

			logger.Logf(t, "creating namespaces %s in clients cluster", staticServerNamespace)
			k8s.RunKubectl(t, clientClusterContext.KubectlOptions(t), "create", "ns", staticServerNamespace)
			helpers.Cleanup(t, cfg.NoCleanupOnFailure, func() {
				k8s.RunKubectl(t, clientClusterContext.KubectlOptions(t), "delete", "ns", staticServerNamespace)
			})

			consulClient, _ := serverConsulCluster.SetupConsulClient(t, c.ACLsAndAutoEncryptEnabled)

			serverQueryServerOpts := &api.QueryOptions{Namespace: staticServerNamespace, Partition: defaultPartition}
			serverQueryClientOpts := &api.QueryOptions{Namespace: staticServerNamespace, Partition: secondaryPartition}

			if !c.mirrorK8S {
				serverQueryServerOpts = &api.QueryOptions{Namespace: c.destinationNamespace, Partition: defaultPartition}
				serverQueryClientOpts = &api.QueryOptions{Namespace: c.destinationNamespace, Partition: secondaryPartition}
			}

			// Check that the ACL token is deleted.
			if c.ACLsAndAutoEncryptEnabled {
				// We need to register the cleanup function before we create the deployments
				// because golang will execute them in reverse order i.e. the last registered
				// cleanup function will be executed first.
				t.Cleanup(func() {
					if c.ACLsAndAutoEncryptEnabled {
						retry.Run(t, func(r *retry.R) {
							tokens, _, err := consulClient.ACL().TokenList(serverQueryServerOpts)
							require.NoError(r, err)
							for _, token := range tokens {
								require.NotContains(r, token.Description, staticServerName)
							}

							tokens, _, err = consulClient.ACL().TokenList(serverQueryClientOpts)
							require.NoError(r, err)
							for _, token := range tokens {
								require.NotContains(r, token.Description, staticServerName)
							}
						})
					}
				})
			}

			logger.Log(t, "creating a static-server with a service")
			// create service in default partition.
			k8s.DeployKustomize(t, serverClusterStaticServerOpts, cfg.NoCleanupOnFailure, cfg.DebugDirectory, "../fixtures/bases/static-server")
			// create service in secondary partition.
			k8s.DeployKustomize(t, clientClusterStaticServerOpts, cfg.NoCleanupOnFailure, cfg.DebugDirectory, "../fixtures/bases/static-server")

			logger.Log(t, "checking that the service has been synced to Consul")
			var services map[string][]string
			counter := &retry.Counter{Count: 20, Wait: 30 * time.Second}
			retry.RunWith(counter, t, func(r *retry.R) {
				var err error
				// list services in default partition catalog.
				services, _, err = consulClient.Catalog().Services(serverQueryServerOpts)
				require.NoError(r, err)
				require.Contains(r, services, staticServerName)
				if _, ok := services[staticServerName]; !ok {
					r.Errorf("service '%s' is not in Consul's list of services %s in the default partition", staticServerName, services)
				}
				// list services in secondary partition catalog.
				services, _, err = consulClient.Catalog().Services(serverQueryClientOpts)
				require.NoError(r, err)
				require.Contains(r, services, staticServerName)
				if _, ok := services[staticServerName]; !ok {
					r.Errorf("service '%s' is not in Consul's list of services %s in the secondary partition", staticServerName, services)
				}
			})
			// validate service in the default partition.
			service, _, err := consulClient.Catalog().Service(staticServerName, "", serverQueryServerOpts)
			require.NoError(t, err)
			require.Equal(t, 1, len(service))
			require.Equal(t, []string{"k8s"}, service[0].ServiceTags)
			// validate service in the secondary partition.
			service, _, err = consulClient.Catalog().Service(staticServerName, "", serverQueryClientOpts)
			require.NoError(t, err)
			require.Equal(t, 1, len(service))
			require.Equal(t, []string{"k8s"}, service[0].ServiceTags)

		})
	}
}
