package uarewrite

import (
	"strings"
	"testing"
)

// Generated from known bytes: CIDv1 = 0x01 <codec raw 0x55> <multihash>, with
// sha2-256 (0x12,0x20), sha1 (0x11,0x14) and identity (0x00,0x05) digests;
// CIDv0 = 0x12 0x20 <sha2-256> in bare base58btc.
const (
	cidV1B32         = "bafkreig3ae7hkpasu5ecyu6qvpdsc4nclvwsw6n7pldvzxomznp6jsxogi"
	cidV1B32Hex      = "v05ah486r04v7af0ikt42okuglf3i2sd2blmimudvfb3lpnecpdfu9ine68"
	cidV1B36         = "k2cwuee3wptgvgtts3oerb9ujp4tly7gwbs1o8ixvh7g9hzhjb2n9h6a"
	cidV1B16         = "f01551220db013e753c12a7482c53d0abc72171a25d6d2b79bf7ac75cddcccb5fe4caee32"
	cidV1B64         = "uAVUSINsBPnU8EqdILFPQq8chcaJdbSt5v3rHXN3My1_kyu4y"
	cidV1B58         = "zb2rhmPBaYpG58m19kYwtEwYbANtXgXoBQ5tivkRNWGdgCSWu"
	cidShortIdentity = "bafkqablimvwgy3y"
	cidSha1          = "bafkrcfb4sudfwz6n5vxr7zybmxkxurniv2nblza"
	cidV0            = "Qmd5Z6Vc2Dsk3xtzxnUergmv8Fy35h5Lyx8g7hcgsQEGKB"
	badLenB32        = "bafkreig3ae7hkpasu5ecyu6qvpdsc4nclvwsw6n7pldvzxomznp6jsxo"
	badVersionB32    = "bajkreig3ae7hkpasu5ecyu6qvpdsc4nclvwsw6n7pldvzxomznp6jsxogi"
)

func TestIsCID(t *testing.T) {
	t.Parallel()

	valid := []string{cidV1B32, cidV1B32Hex, "V" + strings.ToUpper(cidV1B32Hex[1:]), cidV1B36, cidV1B16, cidV1B64, cidV1B58, cidShortIdentity, cidSha1, cidV0,
		"bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi", // real dag-pb dir
		"QmPChd2hVbrJ6bfo3WBcTW4iZnpHm8TEzWkLHmLpXhF68A",              // real CIDv0
		"QmXb3MrXizNzPhUE984EuNQhseNNpr457f6gti7iRbAgfE",              // objkt GIF
		"bafybeigbz7fx2ce65lpky3idh4cae3umix2vgh4jofinzzyss33zhq4gha", // objkt GIF
	}
	for _, s := range valid {
		if !isCID(s) {
			t.Errorf("isCID(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "b", "api", "v0", "QmAbc", "readme.md", "index.html",
		badLenB32, badVersionB32,
		"b" + cidV1B32[1:len(cidV1B32)-3] + "%2F", // percent-encoding
		"Qm" + cidV0[2:len(cidV0)-1] + "0",        // '0' is not base58
		"x" + cidV1B32[1:],                        // unknown multibase prefix
		cidV0[:45],                                // truncated CIDv0
	}
	for _, s := range invalid {
		if isCID(s) {
			t.Errorf("isCID(%q) = true, want false", s)
		}
	}
}
