package cmd

import (
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/MDrollette/i2p-tools/reseed"
	"github.com/codegangsta/cli"
	"github.com/cretz/bine/tor"
	"github.com/cretz/bine/torutil"
	"github.com/cretz/bine/torutil/ed25519"
)

func NewReseedCommand() cli.Command {
	return cli.Command{
		Name:   "reseed",
		Usage:  "Start a reseed server",
		Action: reseedAction,
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "signer",
				Usage: "Your su3 signing ID (ex. something@mail.i2p)",
			},
			cli.StringFlag{
				Name:  "tlsHost",
				Usage: "The public hostname used on your TLS certificate",
			},
			cli.BoolFlag{
				Name:  "onion",
				Usage: "Present an onionv3 address",
			},
			cli.BoolFlag{
				Name:  "singleOnion",
				Usage: "Use a faster, but non-anonymous single-hop onion",
			},
			cli.StringFlag{
				Name:  "onionKey",
				Value: "onion.key",
				Usage: "Specify a path to an ed25519 private key for onion",
			},
			cli.StringFlag{
				Name:  "key",
				Usage: "Path to your su3 signing private key",
			},
			cli.StringFlag{
				Name:  "netdb",
				Usage: "Path to NetDB directory containing routerInfos",
			},
			cli.StringFlag{
				Name:  "tlsCert",
				Usage: "Path to a TLS certificate",
			},
			cli.StringFlag{
				Name:  "tlsKey",
				Usage: "Path to a TLS private key",
			},
			cli.StringFlag{
				Name:  "ip",
				Value: "0.0.0.0",
				Usage: "IP address to listen on",
			},
			cli.StringFlag{
				Name:  "port",
				Value: "8443",
				Usage: "Port to listen on",
			},
			cli.IntFlag{
				Name:  "numRi",
				Value: 77,
				Usage: "Number of routerInfos to include in each su3 file",
			},
			cli.IntFlag{
				Name:  "numSu3",
				Value: 0,
				Usage: "Number of su3 files to build (0 = automatic based on size of netdb)",
			},
			cli.StringFlag{
				Name:  "interval",
				Value: "90h",
				Usage: "Duration between SU3 cache rebuilds (ex. 12h, 15m)",
			},
			cli.StringFlag{
				Name:  "prefix",
				Value: "",
				Usage: "Prefix path for the HTTP(S) server. (ex. /netdb)",
			},
			cli.BoolFlag{
				Name:  "trustProxy",
				Usage: "If provided, we will trust the 'X-Forwarded-For' header in requests (ex. behind cloudflare)",
			},
			cli.StringFlag{
				Name:  "blacklist",
				Value: "",
				Usage: "Path to a txt file containing a list of IPs to deny connections from.",
			},
			cli.DurationFlag{
				Name:  "stats",
				Value: 0,
				Usage: "Periodically print memory stats.",
			},
		},
	}
}

func reseedAction(c *cli.Context) {
	// validate flags
	netdbDir := c.String("netdb")
	if netdbDir == "" {
		fmt.Println("--netdb is required")
		return
	}

	signerID := c.String("signer")
	if signerID == "" {
		fmt.Println("--signer is required")
		return
	}

	var tlsCert, tlsKey string
	tlsHost := c.String("tlsHost")

	if c.Bool("onion") {
		var ok []byte
		var err error
		if tlsHost == "" {
			if _, err = os.Stat(c.String("onionKey")); err == nil {
				ok, err = ioutil.ReadFile(c.String("onionKey"))
				if err != nil {
					log.Fatalln(err.Error())
				}
			} else {
				key, err := ed25519.GenerateKey(nil)
				if err != nil {
					log.Fatalln(err.Error())
				}
				ok = []byte(key.PrivateKey())
			}
			tlsHost = torutil.OnionServiceIDFromPrivateKey(ed25519.PrivateKey(ok)) + ".onion"
		}
		err = ioutil.WriteFile(c.String("onionKey"), ok, 0644)
		if err != nil {
			log.Fatalln(err.Error())
		}
	}

	if tlsHost != "" {
		tlsKey = c.String("tlsKey")
		// if no key is specified, default to the host.pem in the current dir
		if tlsKey == "" {
			tlsKey = tlsHost + ".pem"
		}

		tlsCert = c.String("tlsCert")
		// if no certificate is specified, default to the host.crt in the current dir
		if tlsCert == "" {
			tlsCert = tlsHost + ".crt"
		}

		// prompt to create tls keys if they don't exist?
		err := checkOrNewTLSCert(tlsHost, &tlsCert, &tlsKey)
		if nil != err {
			log.Fatalln(err)
		}
	}

	reloadIntvl, err := time.ParseDuration(c.String("interval"))
	if nil != err {
		fmt.Printf("'%s' is not a valid time interval.\n", reloadIntvl)
		return
	}

	signerKey := c.String("key")
	// if no key is specified, default to the signerID.pem in the current dir
	if signerKey == "" {
		signerKey = signerFile(signerID) + ".pem"
	}

	// load our signing privKey
	privKey, err := getOrNewSigningCert(&signerKey, signerID)
	if nil != err {
		log.Fatalln(err)
	}

	// create a local file netdb provider
	netdb := reseed.NewLocalNetDb(netdbDir)

	// create a reseeder
	reseeder := reseed.NewReseeder(netdb)
	reseeder.SigningKey = privKey
	reseeder.SignerID = []byte(signerID)
	reseeder.NumRi = c.Int("numRi")
	reseeder.NumSu3 = c.Int("numSu3")
	reseeder.RebuildInterval = reloadIntvl
	reseeder.Start()

	// create a server
	server := reseed.NewServer(c.String("prefix"), c.Bool("trustProxy"))
	server.Reseeder = reseeder
	server.Addr = net.JoinHostPort(c.String("ip"), c.String("port"))

	// load a blacklist
	blacklist := reseed.NewBlacklist()
	server.Blacklist = blacklist
	blacklistFile := c.String("blacklist")
	if "" != blacklistFile {
		blacklist.LoadFile(blacklistFile)
	}

	// print stats once in a while
	if c.Duration("stats") != 0 {
		go func() {
			var mem runtime.MemStats
			for range time.Tick(c.Duration("stats")) {
				runtime.ReadMemStats(&mem)
				log.Printf("TotalAllocs: %d Kb, Allocs: %d Kb, Mallocs: %d, NumGC: %d", mem.TotalAlloc/1024, mem.Alloc/1024, mem.Mallocs, mem.NumGC)
			}
		}()
	}

	if c.Bool("onion") {
		port, err := strconv.Atoi(c.String("port"))
		if err != nil {
			log.Fatalln(err.Error())
		}
		if _, err := os.Stat(c.String("onionKey")); err == nil {
			ok, err := ioutil.ReadFile(c.String("onionKey"))
			if err != nil {
				log.Fatalln(err.Error())
			} else {
				if tlsCert != "" && tlsKey != "" {
					log.Fatalln(
						server.ListenAndServeOnionTLS(
							nil,
							&tor.ListenConf{
								LocalPort:    port,
								Key:          ed25519.PrivateKey(ok),
								RemotePorts:  []int{443},
								Version3:     true,
								NonAnonymous: c.Bool("singleOnion"),
								DiscardKey:   false,
							},
							tlsCert, tlsKey,
						),
					)
				} else {
					log.Fatalln(
						server.ListenAndServeOnion(
							nil,
							&tor.ListenConf{
								LocalPort:    port,
								Key:          ed25519.PrivateKey(ok),
								RemotePorts:  []int{80},
								Version3:     true,
								NonAnonymous: c.Bool("singleOnion"),
								DiscardKey:   false,
							},
						),
					)
				}
			}
		} else if os.IsNotExist(err) {
			log.Fatalln(
				server.ListenAndServeOnion(
					nil,
					&tor.ListenConf{
						LocalPort:    port,
						RemotePorts:  []int{80},
						Version3:     true,
						NonAnonymous: c.Bool("singleOnion"),
						DiscardKey:   false,
					},
				),
			)
		} else {

		}
	} else if tlsHost != "" && tlsCert != "" && tlsKey != "" {
		log.Printf("HTTPS server started on %s\n", server.Addr)
		log.Fatalln(server.ListenAndServeTLS(tlsCert, tlsKey))
	} else {
		log.Printf("HTTP server started on %s\n", server.Addr)
		log.Fatalln(server.ListenAndServe())
	}
}
