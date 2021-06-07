package controllers

import (
	"context"
	"testing"

	"github.com/mysql/ndb-operator/pkg/helpers/testutils"
	"github.com/mysql/ndb-operator/pkg/resources"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMysqlRootPasswordSecretInterface_EnsureSecret(t *testing.T) {

	ns := metav1.NamespaceDefault
	ndb := testutils.NewTestNdb(ns, "test", 2)

	// Create fixture and start informers
	f := newFixture(t, ndb)
	defer f.close()
	f.start()

	sci := NewMySQLRootPasswordSecretInterface(f.kubeclient, ns)

	// Test the secret control interface for default random password
	secret, err := sci.EnsureSecret(context.TODO(), ndb)
	if err != nil {
		t.Errorf("Error ensuring secret : %v", err)
	}
	if secret == nil {
		t.Error("Error ensuring secret : secret is nil")
	}

	// expect one create action
	f.expectCreateAction(ns, "", "v1", "secrets", secret)

	// Test custom secret ensuring when the secret doesn't exist
	customSecretName := "custom-mysqld-root-password"
	ndb.Spec.Mysqld.RootPasswordSecretName = customSecretName
	// Ensuring should fail
	secret, err = sci.EnsureSecret(context.TODO(), ndb)
	if err == nil {
		t.Errorf("Expected '%s' secret not found error but got no error", customSecretName)
	} else if !errors.IsNotFound(err) {
		t.Errorf("Expected '%s' secret not found error but got : %v", customSecretName, err)
	}
	// No action is expected

	// Test custom secret ensuring when the secret exists
	// Create the custom secret
	customSecret := resources.NewMySQLRootPasswordSecret(ndb)
	customSecret.Name = customSecretName
	secret, err = f.kubeclient.CoreV1().Secrets(ns).Create(context.TODO(), customSecret, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Error creating custom secret : %v", err)
	}
	f.expectCreateAction(ns, "", "v1", "secrets", secret)

	// Now ensuring should pass
	secret, err = sci.EnsureSecret(context.TODO(), ndb)
	if err != nil {
		t.Errorf("Error ensuring custom secret '%s' : %v", customSecretName, err)
	}
	if secret == nil {
		t.Error("Error ensuring custom secret : secret is nil")
	}
	// No action is expected

	// Validate all the actions
	f.checkActions()
}
