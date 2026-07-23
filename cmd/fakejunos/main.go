// Command fakejunos runs stand-in Junos devices on loopback so jconfig can be
// tried, demonstrated or load-tested without touching real hardware. It speaks
// both transports jconfig supports and accepts the credentials printed at
// startup.
//
// It is a development aid, not part of the jconfig daemon.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/didww/jconfig/internal/junostest"
)

func main() {
	count := flag.Int("n", 1, "number of fake devices to start")
	dir := flag.String("dir", ".", "directory for the generated known_hosts files")
	flag.Parse()

	for i := 0; i < *count; i++ {
		s, err := junostest.New(*dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fakejunos:", err)
			os.Exit(1)
		}
		defer s.Close()

		fmt.Printf("device %d: host=%s port=%d known_hosts=%s\n",
			i+1, s.Host(), s.Port(), s.KnownHosts())
	}
	fmt.Printf("credentials: username=%s password=%s\n", junostest.User, junostest.Pass)
	fmt.Println("ready; press Ctrl-C to stop")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}
