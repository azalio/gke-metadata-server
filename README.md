gke-metadata-server
===================

A GKE Metadata Server *emulator* for facilitating GCP Workload Identity Federation inside non-GKE
Kubernetes clusters, e.g. on-prem, bare-metal, managed Kubernetes from other clouds, etc. This
implementation is heavily inspired by, and deployed in the same fashion of the Google-managed
`gke-metadata-server` `DaemonSet` present in the `kube-system` Namespace of GKE clusters with
GKE Workload Identity enabled. See how GKE Workload Identity
[works](https://cloud.google.com/kubernetes-engine/docs/concepts/workload-identity#metadata_server).

**Important:** This project was not created by Google or by anybody related to Google.
Google enterprise support is not available, but community support is. Please feel free
to open issues for reporting bugs or vulnerabilities, or for asking questions.
**But use this tool at your own risk.**

## Limitations and Caveats

### Pod identification by IP address

The server uses the source IP address reported in the HTTP request to identify the
requesting Pod in the Kubernetes API through the following `client-go` API call:

```go
func (s *Server) tryGetPod(...) {
    // clientIP is extracted and parsed from r.RemoteAddr
    podList, err := s.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
        FieldSelector: fmt.Sprintf("spec.nodeName=%s,status.podIP=%s", s.opts.NodeName, clientIP),
    })
    // now filter Pods with spec.hostNetwork==false (not supported in the FieldSelector above)
    // and check if exactly one pod remains. if yes, then serve the requested credentials
}
```

If your cluster has an easy surface for an attacker to impersonate a Pod IP address, maybe via [ARP
spoofing](https://cloud.hacktricks.xyz/pentesting-cloud/kubernetes-security/kubernetes-network-attacks),
then the attacker may exploit this behavior to steal credentials from the emulator.
**Please evaluate the risk of such attacks in your cluster before choosing this tool.**

*(Please also note that the attack explained in the link above requires Pods configured
with very high privileges. If you are currently
[capable of enforcing restrictions](https://github.com/open-policy-agent/gatekeeper)
and preventing that kind of configuration then you should be able to greatly reduce the
attack surface.)*

### Pods running on the host network

In a cluster there can also be Pods running on the *host network*, i.e. Pods with the
field `spec.hostNetwork` set to `true`. For example, the emulator itself needs to run in
that mode in order to listen to TCP/IP connections coming from Pods running on the same
Kubernetes Node the emulator Pod is running on. Since Pods running on the host network
share the same IP address, i.e. the IP address of the Node itself where they are running
on, the solution implemented here would not be able to uniquely and securely identify
such Pods by IP address. Therefore, *Pods running on the host network are not supported*.

## Usage

Steps:
1. Configure Kubernetes DNS
2. Configure Kubernetes ServiceAccount OIDC Discovery
3. Configure GCP Workload Identity Federation
4. Deploy `gke-metadata-server` in your cluster
5. (Optional) Verify Supply Chain Security

### Configure Kubernetes DNS

Add the following DNS entry to your cluster:

`metadata.google.internal` ---> `169.254.169.254`

Google libraries and `gcloud` query these well-known endpoints for retrieving Google OAuth 2.0
access tokens, which are short-lived (1h-long) authorization codes granting access to resources
in the GCP APIs.

#### CoreDNS

If your cluster uses CoreDNS, here's a StackOverflow [tutorial](https://stackoverflow.com/a/65338650)
for adding custom cluster-level DNS entries.

Adding an entry to CoreDNS does not work seamlessly for all cases. Depending on how the
application code resolves DNS, the Pod-level DNS configuration mentioned in the link
above may be the only feasible choice. See an example at [`./k8s/test.yaml`](./k8s/test.yaml).

*(Google's Go libraries target the `169.254.169.254` IP address directly. If you are running mostly
Go applications *authenticating through Google's Go libraries* then this DNS configuration may not
be required. Test it!)*

### Configure Kubernetes ServiceAccount OIDC Discovery

Kubernetes supports configuring the OIDC discovery endpoints for the ServiceAccount OIDC Identity Provider
([docs](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/#service-account-issuer-discovery)).

A relying party interested in validating ID tokens for Kubernetes ServiceAccounts (e.g. `sts.googleapis.com`)
first requests the OIDC configuration from `$ISSUER_URI/.well-known/openid-configuration`. The returned JSON
has a field called `jwks_uri` containing a URI for the *JSON Web Key Sets* document, usually of the form
`$ISSUER_URI/openid/v1/jwks`. This second JSON document has the public cryptographic keys that must be used
for verifying the signatures of ServiceAccount Tokens issued by Kubernetes.

The Kubernetes API serves these two documents, but since both can be publicly available it's much safer
to store and serve them from a reliable, publicly and highly available endpoint from where GCP will,
guaranteed, be able to discover the authentication parameters from. For example, public GCS/S3 buckets and
objects, etc. The CLI of the project also offers the command `publish` for uploading the two required JSON
documents to a GCS bucket, but basically this is how you could retrieve the documents from inside a Pod using
`curl` and `gcloud`:

```bash
# fetch the JSON documents from the k8s API server
curl -s --cacert /run/secrets/kubernetes.io/serviceaccount/ca.crt -H "Authorization: Bearer $(cat /run/secrets/kubernetes.io/serviceaccount/token)" "https://kubernetes.default.svc.cluster.local/.well-known/openid-configuration" > ./.well-known/openid-configuration
curl -s --cacert /run/secrets/kubernetes.io/serviceaccount/ca.crt -H "Authorization: Bearer $(cat /run/secrets/kubernetes.io/serviceaccount/token)" "https://kubernetes.default.svc.cluster.local/openid/v1/jwks" > ./openid/v1/jwks

# upload them to a public GCS bucket named $ISSUER_BUCKET
gcloud storage cp ./.well-known/openid-configuration gs://$ISSUER_BUCKET/.well-known/openid-configuration
gcloud storage cp ./openid/v1/jwks gs://$ISSUER_BUCKET/openid/v1/jwks

# their public HTTPS URLs will be respectively:
echo "https://storage.googleapis.com/$ISSUER_BUCKET/.well-known/openid-configuration"
echo "https://storage.googleapis.com/$ISSUER_BUCKET/openid/v1/jwks"
```

If `gcloud` is not configured with the required permissions inside the Pod you can
simply `cat` the files in the terminal, copy their content, exit to your local env
and run the `gcloud` upload commands authenticating with your own local credentials.

Configuring the OIDC Issuer and JWKS URIs usually implies restarting the Kubernetes Control Plane for
specifying the required CLI flags for the API server (e.g. see the KinD development configuration at
[`./kind-config.yaml`](./kind-config.yaml)).

```bash
# the --service-account-issuer k8s API server CLI flag is the $ISSUER_URI (i.e without the
# ./.well-known/openid-configuration last part):
echo "https://storage.googleapis.com/$ISSUER_BUCKET"

# the --service-account-jwks-uri k8s API server CLI flag must be the full URL:
echo "https://storage.googleapis.com/$ISSUER_BUCKET/openid/v1/jwks"
```

### Configure GCP Workload Identity Federation

Steps:
1. Configure Pool and Provider
2. Configure Service Account Impersonation for Kubernetes

Docs: [link](https://cloud.google.com/iam/docs/workload-identity-federation-with-kubernetes).

Examples for all the configurations described in this section are available here:
[`./terraform/main.tf`](./terraform/main.tf). This is where we provision the
infrastructure required for testing this project in CI.

#### Pool and Provider

In order to map Kubernetes ServiceAccounts to Google Service Accounts, one must first create
a Workload Identity Pool and Provider. A Pool is a set of Subjects and a set of Providers,
with each Subject being visible to all the Providers in the set. For enforcing a strict
authentication system, be sure to create exactly one Provider per Pool, i.e. create a single
Pool+PoolProvider pair for each Kubernetes cluster. This Provider must reflect the Kubernetes
ServiceAccounts OIDC Identity provider configured in the previous step. The Issuer URI will
be required (e.g. the HTTPS URL of a publicly available GCS bucket containing an object at
key `.well-known/openid-configuration`), and the following *attribute mapping* rule must be
created:

`google.subject = assertion.sub`

If this configuration is performed correctly, the projected value of `google.subject` mapped
from a Kubernetes ServiceAccount Token will have the following syntax:

`system:serviceaccount:{k8s_namespace}:{k8s_sa_name}`

**Attention 1**: The projected value of `google.subject` can have at most 127 characters
([docs](https://registry.terraform.io/providers/hashicorp/google/latest/docs/resources/iam_workload_identity_pool_provider#google.subject)).

**Attention 2**: Please make sure not to specify any *audiences*. This project uses the
default audience when creating Kubernetes ServiceAccount Tokens (which contains the full
resource name of the Provider, which is a good, strict security rule):

`//iam.googleapis.com/{pool_full_name}/providers/{pool_provider_name}`

Where `{pool_full_name}` assumes the form:

`projects/{gcp_project_number}/locations/global/workloadIdentityPools/{pool_name}`

The Pool full name can be retrieved on its Google Cloud Console webpage.

#### Service Account Impersonation for Kubernetes

For allowing the `{gcp_service_account}@{gcp_project_id}.iam.gserviceaccount.com` Google Service Account
to be impersonated by the Kubernetes ServiceAccount `{k8s_sa_name}` of Namespace `{k8s_namespace}`,
create an IAM Policy Binding for the IAM Role `roles/iam.workloadIdentityUser` and the following membership
string on the respective Google Service Account:

`principal://iam.googleapis.com/{pool_full_name}/subject/system:serviceaccount:{k8s_namespace}:{k8s_sa_name}`

This membership encoding the Kubernetes ServiceAccount will be reflected as a Subject in the Google Cloud
Console webpage of the Pool. It allows a Kubernetes ServiceAccount Token issued for the *audience* of the
Pool Provider (see previous section) to be exchanged for an OAuth 2.0 Access Token for the Google Service
Account.

Add also a second IAM Policy Binding for the IAM Role `roles/iam.serviceAccountOpenIdTokenCreator` and the
membership string below *on the Google Service Account itself*:

`serviceAccount:{gcp_service_account}@{gcp_project_id}.iam.gserviceaccount.com`

This "self-impersonation" IAM Policy Binding is necessary for the
`GET /computeMetadata/v1/instance/service-accounts/*/identity` API to work.
This is because our implementation first exchanges the Kubernetes ServiceAccount
Token for a Google Service Account OAuth 2.0 Access Token, and then exchanges
this Access Token for an OpenID Connect ID Token. *(All these tokens are short-lived.)*

Finally, add the following annotation to the Kuberentes ServiceAccount (just like you would in GKE):

`iam.gke.io/gcp-service-account: {gcp_service_account}@{gcp_project_id}.iam.gserviceaccount.com`

If this bijection between the Google Service Account and the Kubernetes ServiceAccount is correctly
configured and `gke-metadata-server` is properly deployed in your cluster, you're good to go.

### Deploy `gke-metadata-server` in your cluster

A Helm Chart is available in the following Helm OCI Repositories:

1. `matheuscscp/gke-metadata-server-helm` (Docker Hub)
2. `ghcr.io/matheuscscp/gke-metadata-server/helm` (GitHub Container Registry)

See the Helm values API at [`./helm/gke-metadata-server/values.yaml`](./helm/gke-metadata-server/values.yaml).

Alternatively, you can write your own Kubernetes manifests and consume only the container images:

1. `matheuscscp/gke-metadata-server` (Docker Hub)
2. `ghcr.io/matheuscscp/gke-metadata-server/container` (GitHub Container Registry)

### (Optional) Verify Supply Chain Security

Here's how you should validate the authenticity of the `gke-metadata-server` images... WIP

* Container Image Repositories: `WIP`
* Helm OCI Repositories: `WIP`
