package proxy

import (
	"context"
	"log"
	"net"
	"time"
)

type LoggingDialer struct{ d net.Dialer }

func (ld LoggingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	start := time.Now()
	log.Printf("[dial] start %s %s", network, addr)
	c, err := ld.d.DialContext(ctx, network, addr)
	log.Printf("[dial] done  %s %s err=%v in %v", network, addr, err, time.Since(start))
	return c, err
}
