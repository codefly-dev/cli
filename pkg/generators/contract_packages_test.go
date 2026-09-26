package generators

import (
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
)

func multiPackageEndpoint(digest string) composition.APIContractEndpoint {
	return composition.APIContractEndpoint{
		Service: "documents", Endpoint: "grpc", Kind: composition.APIContractKindProtobuf,
		Package: "acme.documents.v1", Digest: digest,
		Services: []composition.APIContractService{
			{Name: "DocumentService", FullName: "acme.documents.v1.DocumentService"},
			{Name: "ReceiptService", FullName: "acme.receipts.v1.ReceiptService"},
		},
	}
}

func TestContractServicePackages(t *testing.T) {
	endpoint := multiPackageEndpoint("sha256:aaaa")
	if got := ContractServicePackages(&endpoint); !slices.Equal(got, []string{"acme.documents.v1", "acme.receipts.v1"}) {
		t.Fatalf("packages = %v, want both service packages", got)
	}

	single := composition.APIContractEndpoint{Package: "acme.billing.v1", Services: []composition.APIContractService{
		{Name: "InvoiceService", FullName: "acme.billing.v1.InvoiceService"},
		{Name: "AdminService", FullName: "acme.billing.v1.AdminService"},
	}}
	if got := ContractServicePackages(&single); !slices.Equal(got, []string{"acme.billing.v1"}) {
		t.Fatalf("packages = %v, want the one package", got)
	}

	bare := composition.APIContractEndpoint{Package: "acme.billing.v1"}
	if got := ContractServicePackages(&bare); !slices.Equal(got, []string{"acme.billing.v1"}) {
		t.Fatalf("packages = %v, want the Package fallback", got)
	}
}

// A second contract that claims the multi-package endpoint's second package at
// another digest is a diamond, even though the two entries' primary packages
// differ.
func TestCheckContractDiamondSeesASecondPackage(t *testing.T) {
	other := composition.APIContractEndpoint{
		Service: "receipts", Endpoint: "grpc", Kind: composition.APIContractKindProtobuf,
		Package: "acme.receipts.v1", Digest: "sha256:bbbb",
		Services: []composition.APIContractService{{Name: "ReceiptService", FullName: "acme.receipts.v1.ReceiptService"}},
	}
	err := checkContractDiamond([]ContractEntry{
		{Endpoint: multiPackageEndpoint("sha256:aaaa"), ModuleName: "documents"},
		{Endpoint: other, ModuleName: "receipts"},
	})
	if err == nil || !strings.Contains(err.Error(), "contract acme.receipts.v1 is consumed at two versions") {
		t.Fatalf("error = %v, want the shared second package reported as a diamond", err)
	}

	if err = checkContractDiamond([]ContractEntry{{Endpoint: multiPackageEndpoint("sha256:aaaa"), ModuleName: "documents"}}); err != nil {
		t.Fatalf("one multi-package entry is not a diamond with itself: %v", err)
	}
}
