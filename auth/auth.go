/*
Copyright 2017 Frederic Branczyk Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"text/template"
	"time"

	"github.com/golang/glog"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/authenticatorfactory"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/authorization/authorizerfactory"
	clientset "k8s.io/client-go/kubernetes"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1beta1"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1beta1"
)

type X509Config struct {
	ClientCAFile string
}

type AuthnConfig struct {
	X509   *X509Config
	Header *AuthnHeaderConfig
}

type AuthzConfig struct {
	Rewrites           *SubjectAccessReviewRewrites `json:"rewrites,omitempty"`
	ResourceAttributes *ResourceAttributes          `json:"resourceAttributes,omitempty"`
}

type SubjectAccessReviewRewrites struct {
	ByQueryParameter *QueryParameterRewriteConfig `json:"byQueryParameter,omitempty"`
}

type QueryParameterRewriteConfig struct {
	Name string `json:"name,omitempty"`
}

type AuthnHeaderConfig struct {
	// When set to true, kube-rbac-proxy adds auth-related fields to the headers of http requests sent to the upstream
	Enabled bool
	// Corresponds to the name of the field inside a http(2) request header
	// to tell the upstream server about the user's name
	UserFieldName string
	// Corresponds to the name of the field inside a http(2) request header
	// to tell the upstream server about the user's groups
	GroupsFieldName string
	// The separator string used for concatenating multiple group names in a groups header field's value
	GroupSeparator string
}

type ResourceAttributes struct {
	Namespace   string `json:"namespace,omitempty"`
	APIGroup    string `json:"apiGroup,omitempty"`
	APIVersion  string `json:"apiVersion,omitempty"`
	Resource    string `json:"resource,omitempty"`
	Subresource string `json:"subresource,omitempty"`
	Name        string `json:"name,omitempty"`
}

type AuthConfig struct {
	Authentication *AuthnConfig
	Authorization  *AuthzConfig
}

func (c *AuthConfig) DeepCopy() *AuthConfig {
	res := &AuthConfig{
		Authentication: &AuthnConfig{},
	}

	if c.Authentication != nil {
		res.Authentication = &AuthnConfig{}

		if c.Authentication.X509 != nil {
			res.Authentication.X509 = &X509Config{
				ClientCAFile: c.Authentication.X509.ClientCAFile,
			}
		}

		if c.Authentication.Header != nil {
			res.Authentication.Header = &AuthnHeaderConfig{
				Enabled:         c.Authentication.Header.Enabled,
				UserFieldName:   c.Authentication.Header.UserFieldName,
				GroupsFieldName: c.Authentication.Header.GroupsFieldName,
				GroupSeparator:  c.Authentication.Header.GroupSeparator,
			}
		}
	}

	if c.Authorization != nil {
		if c.Authorization.ResourceAttributes != nil {
			res.Authorization = &AuthzConfig{
				ResourceAttributes: &ResourceAttributes{
					Namespace:   c.Authorization.ResourceAttributes.Namespace,
					APIGroup:    c.Authorization.ResourceAttributes.APIGroup,
					APIVersion:  c.Authorization.ResourceAttributes.APIVersion,
					Resource:    c.Authorization.ResourceAttributes.Resource,
					Subresource: c.Authorization.ResourceAttributes.Subresource,
					Name:        c.Authorization.ResourceAttributes.Name,
				},
			}
		}
	}

	return res
}

// kubeRBACProxyAuth implements AuthInterface
type kubeRBACProxyAuth struct {
	// authenticator identifies the user for requests to kube-rbac-proxy
	authenticator.Request
	// authorizer determines whether a given authorization.Attributes is allowed
	authorizer.Authorizer
	// authorizerAttributesGetter implements retrieving authorization attributes for a respective request.
	authorizerAttributesGetter *krpAuthorizerAttributesGetter
	// config for kube-rbac-proxy
	Config *AuthConfig
}

func newKubeRBACProxyAuth(authenticator authenticator.Request, authorizer authorizer.Authorizer, authConfig *AuthConfig) *kubeRBACProxyAuth {
	return &kubeRBACProxyAuth{authenticator, authorizer, newKubeRBACProxyAuthorizerAttributesGetter(authConfig.Authorization), authConfig}
}

// BuildAuthHandler creates an authenticator, an authorizer, and a matching authorizer attributes getter compatible with the kube-rbac-proxy
func BuildAuthHandler(client clientset.Interface, config *AuthConfig) (*kubeRBACProxyAuth, error) {
	// Get clients, if provided
	var (
		tokenClient authenticationclient.TokenReviewInterface
		sarClient   authorizationclient.SubjectAccessReviewInterface
	)
	if client != nil && !reflect.ValueOf(client).IsNil() {
		tokenClient = client.AuthenticationV1beta1().TokenReviews()
		sarClient = client.AuthorizationV1beta1().SubjectAccessReviews()
	}

	authenticator, err := buildAuthn(tokenClient, config.Authentication)
	if err != nil {
		return nil, err
	}

	authorizer, err := buildAuthz(sarClient)
	if err != nil {
		return nil, err
	}

	return newKubeRBACProxyAuth(authenticator, authorizer, config), nil
}

// buildAuthn creates an authenticator compatible with the kubelet's needs
func buildAuthn(client authenticationclient.TokenReviewInterface, authn *AuthnConfig) (authenticator.Request, error) {
	authenticatorConfig := authenticatorfactory.DelegatingAuthenticatorConfig{
		Anonymous:    false, // always require authentication
		CacheTTL:     2 * time.Minute,
		ClientCAFile: authn.X509.ClientCAFile,
	}

	if client == nil {
		return nil, errors.New("no client provided, cannot use webhook authentication")
	}
	authenticatorConfig.TokenAccessReviewClient = client

	authenticator, _, err := authenticatorConfig.New()
	return authenticator, err
}

// buildAuthz creates an authorizer compatible with the kubelet's needs
func buildAuthz(client authorizationclient.SubjectAccessReviewInterface) (authorizer.Authorizer, error) {
	if client == nil {
		return nil, errors.New("no client provided, cannot use webhook authorization")
	}
	authorizerConfig := authorizerfactory.DelegatingAuthorizerConfig{
		SubjectAccessReviewClient: client,
		AllowCacheTTL:             5 * time.Minute,
		DenyCacheTTL:              30 * time.Second,
	}
	return authorizerConfig.New()
}

func newKubeRBACProxyAuthorizerAttributesGetter(authzConfig *AuthzConfig) *krpAuthorizerAttributesGetter {
	return &krpAuthorizerAttributesGetter{authzConfig}
}

type krpAuthorizerAttributesGetter struct {
	authzConfig *AuthzConfig
}

// GetRequestAttributes populates authorizer attributes for the requests to kube-rbac-proxy.
func (n krpAuthorizerAttributesGetter) GetRequestAttributes(u user.Info, r *http.Request) []authorizer.Attributes {
	apiVerb := ""
	switch r.Method {
	case "POST":
		apiVerb = "create"
	case "GET":
		apiVerb = "get"
	case "PUT":
		apiVerb = "update"
	case "PATCH":
		apiVerb = "patch"
	case "DELETE":
		apiVerb = "delete"
	}

	allAttrs := []authorizer.Attributes{}

	if n.authzConfig.ResourceAttributes != nil {
		if n.authzConfig.Rewrites != nil && n.authzConfig.Rewrites.ByQueryParameter != nil && n.authzConfig.Rewrites.ByQueryParameter.Name != "" {
			params, ok := r.URL.Query()[n.authzConfig.Rewrites.ByQueryParameter.Name]
			if !ok {
				return nil
			}

			for _, param := range params {
				attrs := authorizer.AttributesRecord{
					User:            u,
					Verb:            apiVerb,
					Namespace:       templateWithValue(n.authzConfig.ResourceAttributes.Namespace, param),
					APIGroup:        templateWithValue(n.authzConfig.ResourceAttributes.APIGroup, param),
					APIVersion:      templateWithValue(n.authzConfig.ResourceAttributes.APIVersion, param),
					Resource:        templateWithValue(n.authzConfig.ResourceAttributes.Resource, param),
					Subresource:     templateWithValue(n.authzConfig.ResourceAttributes.Subresource, param),
					Name:            templateWithValue(n.authzConfig.ResourceAttributes.Name, param),
					ResourceRequest: true,
				}
				allAttrs = append(allAttrs, attrs)
			}
		} else {
			attrs := authorizer.AttributesRecord{
				User:            u,
				Verb:            apiVerb,
				Namespace:       n.authzConfig.ResourceAttributes.Namespace,
				APIGroup:        n.authzConfig.ResourceAttributes.APIGroup,
				APIVersion:      n.authzConfig.ResourceAttributes.APIVersion,
				Resource:        n.authzConfig.ResourceAttributes.Resource,
				Subresource:     n.authzConfig.ResourceAttributes.Subresource,
				Name:            n.authzConfig.ResourceAttributes.Name,
				ResourceRequest: true,
			}
			allAttrs = append(allAttrs, attrs)
		}
	} else {
		requestPath := r.URL.Path
		// Default attributes mirror the API attributes that would allow this access to kube-rbac-proxy
		attrs := authorizer.AttributesRecord{
			User:            u,
			Verb:            apiVerb,
			Namespace:       "",
			APIGroup:        "",
			APIVersion:      "",
			Resource:        "",
			Subresource:     "",
			Name:            "",
			ResourceRequest: false,
			Path:            requestPath,
		}
		allAttrs = append(allAttrs, attrs)
	}

	for attrs := range allAttrs {
		glog.V(5).Infof("kube-rbac-proxy request attributes: attrs=%#v", attrs)
	}

	return allAttrs
}

type AuthHandler interface {
	Handle(w http.ResponseWriter, req *http.Request) bool
}

// Handle authenticates the client and authorizes the request.
// If the authn fails, a 401 error is returned. If the authz fails, a 403 error is returned
func (h *kubeRBACProxyAuth) Handle(w http.ResponseWriter, req *http.Request) bool {
	// Authenticate
	u, ok, err := h.AuthenticateRequest(req)
	if err != nil {
		glog.Errorf("Unable to authenticate the request due to an error: %v", err)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}

	// Get authorization attributes
	allAttrs := h.authorizerAttributesGetter.GetRequestAttributes(u, req)
	if len(allAttrs) == 0 {
		msg := fmt.Sprintf("Bad Request. The request or configuration is malformed.")
		glog.V(2).Info(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return false
	}

	for _, attrs := range allAttrs {
		// Authorize
		authorized, _, err := h.Authorize(attrs)
		if err != nil {
			msg := fmt.Sprintf("Authorization error (user=%s, verb=%s, resource=%s, subresource=%s)", u.GetName(), attrs.GetVerb(), attrs.GetResource(), attrs.GetSubresource())
			glog.Errorf(msg, err)
			http.Error(w, msg, http.StatusInternalServerError)
			return false
		}
		if authorized != authorizer.DecisionAllow {
			msg := fmt.Sprintf("Forbidden (user=%s, verb=%s, resource=%s, subresource=%s)", u.GetName(), attrs.GetVerb(), attrs.GetResource(), attrs.GetSubresource())
			glog.V(2).Info(msg)
			http.Error(w, msg, http.StatusForbidden)
			return false
		}
	}

	if h.Config.Authentication.Header.Enabled {
		// Seemingly well-known headers to tell the upstream about user's identity
		// so that the upstream can achieve the original goal of delegating RBAC authn/authz to kube-rbac-proxy
		headerCfg := h.Config.Authentication.Header
		req.Header.Set(headerCfg.UserFieldName, u.GetName())
		req.Header.Set(headerCfg.GroupsFieldName, strings.Join(u.GetGroups(), headerCfg.GroupSeparator))
	}

	return true
}

func templateWithValue(templateString, value string) string {
	tmpl, _ := template.New("valueTemplate").Parse(templateString)
	out := bytes.NewBuffer(nil)
	tmpl.Execute(out, struct{ Value string }{Value: value})

	return out.String()
}
