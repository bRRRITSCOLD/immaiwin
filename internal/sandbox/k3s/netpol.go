package k3s

import (
	"context"
	"fmt"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// Pod label values used by NetworkPolicy selectors.
const (
	netLabel      = sandbox.SandboxLabel + "/network"
	netLabelAllow = "allow"
	netLabelDeny  = "deny"
)

// CIDRs holds the cluster CIDRs blocked by the egress-only policy. Distros
// vary (k3s vs kubeadm vs managed clouds) so callers configure these.
type CIDRs struct {
	Pod       string // pod CIDR, e.g. "10.42.0.0/16"
	Service   string // service CIDR, e.g. "10.43.0.0/16"
	LinkLocal string // link-local + cloud metadata, e.g. "169.254.0.0/16"
}

// ensureNetworkPolicies installs deny-all + egress-only policies in the
// sandbox namespace. Idempotent (upserts on each call).
//
// Posture:
//   - Pods labeled "immaiwin.sandbox/network=deny" → all ingress + egress denied.
//   - Pods labeled "immaiwin.sandbox/network=allow" → ingress denied; egress
//     limited to DNS (kube-system) + 0.0.0.0/0 minus pod/service/link-local CIDRs.
func ensureNetworkPolicies(ctx context.Context, cli kubernetes.Interface, ns string, cidrs CIDRs) error {
	deny := buildDenyAllPolicy(ns)
	allow := buildEgressOnlyPolicy(ns, cidrs)
	if err := upsertNetworkPolicy(ctx, cli, ns, deny); err != nil {
		return fmt.Errorf("upsert deny policy: %w", err)
	}
	if err := upsertNetworkPolicy(ctx, cli, ns, allow); err != nil {
		return fmt.Errorf("upsert allow policy: %w", err)
	}
	return nil
}

func upsertNetworkPolicy(ctx context.Context, cli kubernetes.Interface, ns string, np *netv1.NetworkPolicy) error {
	existing, err := cli.NetworkingV1().NetworkPolicies(ns).Get(ctx, np.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, cerr := cli.NetworkingV1().NetworkPolicies(ns).Create(ctx, np, metav1.CreateOptions{})
		if cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return cerr
		}
		return nil
	}
	if err != nil {
		return err
	}
	np.ResourceVersion = existing.ResourceVersion
	_, err = cli.NetworkingV1().NetworkPolicies(ns).Update(ctx, np, metav1.UpdateOptions{})
	return err
}

func buildDenyAllPolicy(ns string) *netv1.NetworkPolicy {
	return &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-deny-all",
			Namespace: ns,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					sandbox.SandboxLabel: "true",
					netLabel:             netLabelDeny,
				},
			},
			PolicyTypes: []netv1.PolicyType{
				netv1.PolicyTypeIngress,
				netv1.PolicyTypeEgress,
			},
			// Empty Ingress/Egress = deny all.
		},
	}
}

func buildEgressOnlyPolicy(ns string, cidrs CIDRs) *netv1.NetworkPolicy {
	exceptions := []string{}
	for _, c := range []string{cidrs.Pod, cidrs.Service, cidrs.LinkLocal} {
		if c != "" {
			exceptions = append(exceptions, c)
		}
	}
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	port53 := intstr.FromInt(53)

	return &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-egress-only",
			Namespace: ns,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					sandbox.SandboxLabel: "true",
					netLabel:             netLabelAllow,
				},
			},
			PolicyTypes: []netv1.PolicyType{
				netv1.PolicyTypeIngress,
				netv1.PolicyTypeEgress,
			},
			// No Ingress entries = deny ingress.
			Egress: []netv1.NetworkPolicyEgressRule{
				{
					// DNS to kube-system pods.
					To: []netv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
						},
					}},
					Ports: []netv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &port53},
						{Protocol: &tcp, Port: &port53},
					},
				},
				{
					// External egress, minus cluster CIDRs and link-local.
					To: []netv1.NetworkPolicyPeer{{
						IPBlock: &netv1.IPBlock{
							CIDR:   "0.0.0.0/0",
							Except: exceptions,
						},
					}},
				},
			},
		},
	}
}
