// Package cases lists the v1 contract cases: the requests old clients
// send. cmd/legacy records them against the old service; the v1
// contract test replays them against the adapter. Names sort in run
// order (the discount case writes, so it runs after the reads).
package cases

import (
	"encoding/json"

	"github.com/iodesystems/gwag/v2/gw/contract"
)

// V1 returns the v1 cases.
func V1() []contract.Case {
	return []contract.Case{
		{Name: "01-get-order", Path: "/v1/orders/o-1"},
		{Name: "02-get-closed-order", Path: "/v1/orders/o-2"},
		{Name: "03-get-missing-order", Path: "/v1/orders/o-404"},
		{Name: "04-list-orders", Path: "/v1/orders"},
		{Name: "05-list-orders-min-amount", Path: "/v1/orders?minAmount=10"},
		{Name: "06-discount-out-of-range", Method: "POST", Path: "/v1/orders/o-1/discount", Body: json.RawMessage(`{"percent":0}`)},
		{Name: "07-discount-closed-order", Method: "POST", Path: "/v1/orders/o-2/discount", Body: json.RawMessage(`{"percent":10}`)},
		{Name: "08-discount-rounds", Method: "POST", Path: "/v1/orders/o-1/discount", Body: json.RawMessage(`{"percent":15}`)},
	}
}
