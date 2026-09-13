// Package main implements the KubeHealth Crossplane Composition Function.
package main

import (
	"time"

	"github.com/alecthomas/kong"
	"github.com/kubehealth-project/kubehealth/catalog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	function "github.com/crossplane/function-sdk-go"
)

// CLI configures the function server.
type CLI struct {
	Debug bool `help:"Emit debug logs in addition to info logs." short:"d"`

	Network            string `default:"tcp" help:"Network on which to listen for gRPC connections."`
	Address            string `default:":9443" help:"Address at which to listen for gRPC connections."`
	TLSCertsDir        string `env:"TLS_SERVER_CERTS_DIR" help:"Directory containing tls.key, tls.crt, and ca.crt."`
	Insecure           bool   `help:"Run without mTLS credentials. Use only for local development."`
	MaxRecvMessageSize int    `default:"4" help:"Maximum size of received messages in MB."`
}

// Run starts the function server.
func (c *CLI) Run() error {
	log, err := function.NewLogger(c.Debug)
	if err != nil {
		return err
	}

	assessor, err := catalog.NewAssessor()
	if err != nil {
		return err
	}

	return function.Serve(
		NewFunction(log, assessor, func() metav1.Time { return metav1.NewTime(time.Now()) }),
		function.Listen(c.Network, c.Address),
		function.MTLSCertificates(c.TLSCertsDir),
		function.Insecure(c.Insecure),
		function.MaxRecvMessageSize(c.MaxRecvMessageSize*1024*1024),
	)
}

func main() {
	ctx := kong.Parse(&CLI{}, kong.Description("Propagate KubeHealth from composed resources to a Crossplane XR."))
	ctx.FatalIfErrorf(ctx.Run())
}
