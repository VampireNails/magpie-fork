package remotejoin

import "testing"

func TestOwnerJSONRejectsDuplicateKeysAtEveryDepth(t *testing.T) {
	cases := []struct {
		name, data string
		target     any
	}{
		{"discovery_same", `{"generation":"a","generation":"a"}`, &ownerDiscovery{}},
		{"discovery_conflict", `{"generation":"a","generation":"b"}`, &ownerDiscovery{}},
		{"request_nested_same", `{"command":{"operation":"status","operation":"status"}}`, &ownerRequest{}},
		{"request_nested_conflict", `{"command":{"operation":"status","operation":"disconnect"}}`, &ownerRequest{}},
		{"reply_nested_same", `{"reply":{"state":"x","status":{"owner":true,"owner":true}}}`, &ownerResult{}},
		{"reply_nested_conflict", `{"reply":{"state":"x","status":{"connected":true,"connected":false}}}`, &ownerResult{}},
		{"discovery_case_variant", `{"generation":"a","Generation":"a"}`, &ownerDiscovery{}},
		{"discovery_escaped", `{"generation":"a","gener\u0061tion":"a"}`, &ownerDiscovery{}},
		{"array_nested", `{"items":[{"key":1,"key":1}]}`, &map[string]any{}},
		{"unicode_fold_same", `{"reply":{"status":{"state":"x","ſtate":"x"}}}`, &ownerResult{}},
		{"unicode_fold_conflict", `{"reply":{"status":{"state":"x","ſtate":"y"}}}`, &ownerResult{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if decodeOwnerJSON([]byte(tc.data), tc.target) == nil {
				t.Fatal("duplicate key accepted")
			}
		})
	}
}

func TestOwnerJSONAcceptsDistinctKeysInSeparateObjects(t *testing.T) {
	var out map[string]any
	if err := decodeOwnerJSON([]byte(`{"items":[{"key":1},{"key":2}]}`), &out); err != nil {
		t.Fatal(err)
	}
}
