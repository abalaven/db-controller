package controllers

import (
	"bytes"
	"testing"

	"github.com/go-logr/logr"
	"github.com/spf13/viper"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var complexityEnabled = []byte(`
    passwordConfig:
      passwordComplexity: enabled
      minPasswordLength: "15"
      passwordRotationPeriod: "60"
`)

var complexityDisabled = []byte(`
    passwordConfig:
      passwordComplexity: disabled
      minPasswordLength: "15"
      passwordRotationPeriod: "60"
`)

func TestDatabaseClaimReconciler_generatePassword(t *testing.T) {
	type reconciler struct {
		Client client.Client
		Log    logr.Logger
		Scheme *runtime.Scheme
		Config *viper.Viper
	}
	tests := []struct {
		name    string
		rec     reconciler
		want    int
		wantErr bool
	}{
		{
			"Generate passwordComplexity enabled",
			reconciler{
				Config: NewConfig(complexityEnabled),
			},
			15,
			false,
		},
		{
			"Generate passwordComplexity disabled",
			reconciler{
				Config: NewConfig(complexityDisabled),
			},
			defaultPassLen,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &DatabaseClaimReconciler{
				Client: tt.rec.Client,
				Log:    tt.rec.Log,
				Scheme: tt.rec.Scheme,
				Config: tt.rec.Config,
			}
			got, err := r.generatePassword()
			if (err != nil) != tt.wantErr {
				t.Errorf("generatePassword() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(got) != tt.want {
				t.Errorf("generatePassword() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func NewConfig(in []byte) *viper.Viper {
	c := viper.NewWithOptions(viper.KeyDelimiter("::"))
	c.SetConfigType("yaml")
	c.ReadConfig(bytes.NewBuffer(in))

	return c
}