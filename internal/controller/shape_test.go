package controller

import (
	"strings"
	"testing"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

func iops(v int32) *int32 { return &v }

// instanceTypes mirrors what RavenDB Cloud publishes: a range on the class that
// charges per IOPS, nothing on the class that does not.
func instanceTypes() []cloudapi.InstanceType {
	return []cloudapi.InstanceType{{
		Name:         "PB10",
		Tier:         cloudapi.TierProduction,
		DiskSizesGiB: []int32{10, 20, 30, 50, 75},
		StorageOffers: []cloudapi.StorageOffer{
			{Type: cloudapi.StorageSsdStandard},
			{Type: cloudapi.StorageSsdPremium, IOPS: &cloudapi.IOPSRange{Min: 500, Max: 16000}},
		},
	}}
}

func TestValidateStorageIOPS(t *testing.T) {
	cases := []struct {
		name    string
		storage v1alpha1.StorageSpec
		wantErr string
	}{
		{
			name:    "a class you may not tune, left alone",
			storage: v1alpha1.StorageSpec{SizeGB: 50, Type: v1alpha1.StorageSsdStandard},
		},
		{
			name:    "a class you may not tune, tuned anyway",
			storage: v1alpha1.StorageSpec{SizeGB: 50, Type: v1alpha1.StorageSsdStandard, IOPS: iops(3000)},
			wantErr: "cannot be set for SsdStandard",
		},
		{
			// The defect this stage exists for: RavenDB Cloud refuses a create
			// that leaves iops unset on a class whose range it publishes.
			name:    "a class you must tune, untuned",
			storage: v1alpha1.StorageSpec{SizeGB: 50, Type: v1alpha1.StorageSsdPremium},
			wantErr: "is required for SsdPremium",
		},
		{
			name:    "a class you must tune, tuned",
			storage: v1alpha1.StorageSpec{SizeGB: 50, Type: v1alpha1.StorageSsdPremium, IOPS: iops(4000)},
		},
		{
			name:    "below the range",
			storage: v1alpha1.StorageSpec{SizeGB: 50, Type: v1alpha1.StorageSsdPremium, IOPS: iops(100)},
			wantErr: "accepts 500 to 16000",
		},
		{
			name:    "above the range",
			storage: v1alpha1.StorageSpec{SizeGB: 50, Type: v1alpha1.StorageSsdPremium, IOPS: iops(99000)},
			wantErr: "accepts 500 to 16000",
		},
		{
			// An unset type is SsdStandard, so a minimal manifest needs no iops.
			name:    "type unset falls back to the default class",
			storage: v1alpha1.StorageSpec{SizeGB: 50},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStorage(instanceTypes(), "PB10", tc.storage, v1alpha1.ProviderAWS)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// An empty list means this deployment reported nothing, not that nothing
// is allowed, so the request passes and the cloud judges it.
func TestValidateStorageFallsThroughOnAnEmptyList(t *testing.T) {
	if err := validateStorage(nil, "PB10", v1alpha1.StorageSpec{SizeGB: 7}, v1alpha1.ProviderAWS); err != nil {
		t.Errorf("an unknown shape should not be refused here: %v", err)
	}
	shapeWithNoOffers := []cloudapi.InstanceType{{Name: "PB10", Tier: cloudapi.TierProduction}}
	if err := validateStorage(shapeWithNoOffers, "PB10", v1alpha1.StorageSpec{SizeGB: 7, IOPS: iops(9)}, v1alpha1.ProviderAWS); err != nil {
		t.Errorf("a shape reporting no disk menu should not be refused here: %v", err)
	}
}

// The two limits RavenDB Cloud does not publish, so they are copied constants
// and worth pinning against the citations in shape.go.

func TestValidateReleaseChannel(t *testing.T) {
	// The names are versioned in production: "Stable" and "LTS" are not
	// channels, "Stable72" and "Beta50" are. That is the whole reason this
	// check exists, so the cases use the real shape.
	withChannels := &v1alpha1.CloudAccount{
		Status: v1alpha1.CloudAccountStatus{
			ReleaseChannels: []v1alpha1.ReleaseChannelStatus{
				{Name: "Stable72"}, {Name: "Stable62"}, {Name: "Beta50"},
			},
		},
	}

	cases := []struct {
		name     string
		account  *v1alpha1.CloudAccount
		channel  string
		accepted bool
		mentions string
	}{
		{"a channel the account offers", withChannels, "Stable72", true, ""},
		{"the friendly name nobody has", withChannels, "Stable", false, "Stable72"},
		{"an invented one", withChannels, "LTS", false, "not offered"},
		{
			// Same rule as an unknown region: an account whose channels have
			// not been fetched yet must not block a create on data we lack.
			name:     "nothing cached yet, so the cloud judges",
			account:  &v1alpha1.CloudAccount{},
			channel:  "anything",
			accepted: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReleaseChannel(tc.account, tc.channel, "spec.releaseChannel")
			if tc.accepted {
				if err != nil {
					t.Fatalf("%q should be accepted: %v", tc.channel, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%q should be refused", tc.channel)
			}
			if tc.mentions != "" && !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("message does not mention %q: %v", tc.mentions, err)
			}
			// A rejection has to say what to write instead, or it is just a no.
			if !strings.Contains(err.Error(), "spec.releaseChannel") {
				t.Errorf("message does not name the field to change: %v", err)
			}
		})
	}
}
