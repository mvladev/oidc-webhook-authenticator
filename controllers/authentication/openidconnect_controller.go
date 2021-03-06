// SPDX-FileCopyrightText: 2021 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package authentication

import (
	"context"
	"strings"
	"sync"
	"time"

	authenticationv1alpha1 "github.com/gardener/oidc-webhook-authenticator/apis/authentication/v1alpha1"
	"github.com/gardener/oidc-webhook-authenticator/forked/k8s.io/apiserver/plugin/pkg/authenticator/token/oidc"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// OpenIDConnectReconciler reconciles a OpenIDConnect object
type OpenIDConnectReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
	*unionAuthTokenHandler
}

// +kubebuilder:rbac:groups=authentication.gardener.cloud,resources=openidconnects,verbs=get;list;watch

func (r *OpenIDConnectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("openidconnect", req.Name)

	log.Info("Reconciling")
	defer log.Info("Reconcile finished")

	config := &authenticationv1alpha1.OpenIDConnect{}

	err := r.Get(ctx, req.NamespacedName, config)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.handlers.Delete(req.Name)

			return reconcile.Result{}, nil
		}
	}

	if config.DeletionTimestamp != nil {
		log.Info("Deletion timestamp present - removing OIDC authenticator")
		r.handlers.Delete(req.Name)

		return reconcile.Result{}, nil
	}

	algs := make([]string, 0, len(config.Spec.SupportedSigningAlgs))

	for _, alg := range config.Spec.SupportedSigningAlgs {
		algs = append(algs, string(alg))
	}

	opts := oidc.Options{
		ClientID:             config.Spec.ClientID,
		IssuerURL:            config.Spec.IssuerURL,
		RequiredClaims:       config.Spec.RequiredClaims,
		SupportedSigningAlgs: algs,
	}

	if config.Spec.GroupsClaim != nil {
		opts.GroupsClaim = *config.Spec.GroupsClaim
	}

	if config.Spec.GroupsPrefix != nil {
		if *config.Spec.GroupsPrefix != authenticationv1alpha1.ClaimPrefixingDisabled {
			opts.GroupsPrefix = *config.Spec.GroupsPrefix
		}
	} else {
		opts.GroupsPrefix = config.Name + "/"
	}

	if config.Spec.UsernameClaim != nil {
		opts.UsernameClaim = *config.Spec.UsernameClaim
	}

	if config.Spec.UsernamePrefix != nil {
		if *config.Spec.UsernamePrefix != authenticationv1alpha1.ClaimPrefixingDisabled {
			opts.UsernamePrefix = *config.Spec.UsernamePrefix
		}
	} else {
		opts.UsernamePrefix = config.Name + "/"
	}

	auth, err := oidc.NewForkedAuthenticator(oidc.OptionsForked{
		CA:      config.Spec.CABundle,
		Options: opts,
	})
	if err != nil {
		log.Info("Invalid OIDC authenticator, removing it from store")

		r.handlers.Delete(req.Name)

		return reconcile.Result{
			RequeueAfter: time.Second * 10,
		}, err
	}

	r.handlers.Store(req.Name, &authenticatorInfo{
		Token: auth,
		name:  req.Name,
		uid:   config.UID,
	})

	return ctrl.Result{}, nil
}

func (r *OpenIDConnectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.unionAuthTokenHandler == nil {
		r.unionAuthTokenHandler = &unionAuthTokenHandler{handlers: sync.Map{}, log: r.Log}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&authenticationv1alpha1.OpenIDConnect{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 50,
		}).
		Complete(r)
}

// unionAuthTokenHandler authenticates tokens using a chain of authenticator.Token objects
type unionAuthTokenHandler struct {
	handlers sync.Map
	log      logr.Logger
}

// AuthenticateToken authenticates the token using a chain of authenticator.Token objects.
func (u *unionAuthTokenHandler) AuthenticateToken(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	var (
		info    *authenticator.Response
		success bool
	)

	u.handlers.Range(func(key interface{}, value interface{}) bool {
		currAuthRequestHandler, ok := value.(*authenticatorInfo)
		if !ok {
			u.log.Info("cannot convert to authenticatorInfo", "key", key, "value", value)

			return false
		}

		resp, authenticated, err := currAuthRequestHandler.AuthenticateToken(ctx, token)

		done := err == nil && authenticated
		if done {
			userName := resp.User.GetName()
			// Mark token as invalid when userName has "system:" prefix.
			if strings.HasPrefix(userName, "system:") {
				// TODO add logging

				return false
			}

			filteredGroups := []string{}
			for _, group := range resp.User.GetGroups() {
				// ignore groups with "system:" prefix
				if !strings.HasPrefix(group, "system:") {
					filteredGroups = append(filteredGroups, group)
				}
			}

			info = &authenticator.Response{
				User: &user.DefaultInfo{
					Name: userName,
					Extra: map[string][]string{
						"gardener.cloud/authenticator/name": {currAuthRequestHandler.name},
						"gardener.cloud/authenticator/uid":  {string(currAuthRequestHandler.uid)},
					},
					Groups: filteredGroups,
					UID:    resp.User.GetUID(),
				},
			}

			success = true
		}

		return !done
	})

	return info, success, nil
}

type authenticatorInfo struct {
	authenticator.Token
	name string
	uid  types.UID
}
