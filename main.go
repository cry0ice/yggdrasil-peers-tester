package main

import (
	"context"
	"crypto/tls"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/quic-go/quic-go"
)

type Peer struct {
	URL     string
	Latency time.Duration
}

type Peers struct {
	list []Peer
	mu   sync.RWMutex
}

func (p *Peers) Len() int           { return len(p.list) }
func (p *Peers) Swap(i, j int)      { p.list[i], p.list[j] = p.list[j], p.list[i] }
func (p *Peers) Less(i, j int) bool { return p.list[i].Latency < p.list[j].Latency }

var version = "dev"

var (
	displayVersion bool
	includeBad     bool
	includeAvg     bool
	dialTimeout    int
	thread         int
	csvOutput      string
)

var (
	peerNum              int64
	connectablePeerNum   int64
	unconnectablePeerNum int64
)

func init() {
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.BoolVar(&includeBad, "include-bad", false, "Include bad status peers")
	flag.BoolVar(&includeAvg, "include-avg", false, "Include average status peers")
	flag.IntVar(&dialTimeout, "dial-timeout", 5, "Dial timeout (second)")
	flag.IntVar(&thread, "thread", runtime.NumCPU(), "Set thread number to test latency")
	flag.StringVar(&csvOutput, "output", "result.csv", "Set CSV output")
	flag.Parse()
}

func main() {
	if displayVersion {
		fmt.Println(version)
		return
	}

	sourceURL := flag.Arg(0)
	if sourceURL == "" {
		sourceURL = "https://publicpeers.neilalexander.dev/"
	}
	req, err := http.NewRequest(http.MethodGet, sourceURL, nil)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	var peers []string
	for _, s := range doc.Find(".statusgood").EachIter() {
		peers = append(peers, s.Find("#address").Text())
	}
	if includeAvg {
		for _, s := range doc.Find(".statusavg").EachIter() {
			peers = append(peers, s.Find("#address").Text())
		}
	}
	if includeBad {
		for _, s := range doc.Find(".statusbad").EachIter() {
			peers = append(peers, s.Find("#address").Text())
		}
	}

	var wg sync.WaitGroup
	ch := make(chan struct{}, thread)
	peerList := &Peers{}
	for _, p := range peers {
		ch <- struct{}{}
		wg.Add(1)
		peerNum++
		go func() {
			defer wg.Done()
			u, err := url.Parse(p)
			if err != nil {
				log.Println("pinging", p, "error:", err)
				<-ch
				atomic.AddInt64(&unconnectablePeerNum, 1)
				return
			}
			latency, err := getLatency(u)
			if err != nil {
				log.Println("pinging", p, "error:", err)
				atomic.AddInt64(&unconnectablePeerNum, 1)
				<-ch
				return
			}
			atomic.AddInt64(&connectablePeerNum, 1)
			peerList.mu.Lock()
			peerList.list = append(peerList.list, Peer{
				URL:     p,
				Latency: latency,
			})
			peerList.mu.Unlock()
			<-ch
		}()
	}
	wg.Wait()

	sort.Sort(peerList)

	outputFile, err := os.OpenFile(csvOutput, os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer outputFile.Close()
	writer := csv.NewWriter(outputFile)
	if err := writer.Write([]string{"peer", "latency"}); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	for _, p := range peerList.list {
		if err := writer.Write([]string{p.URL, p.Latency.String()}); err != nil {
			fmt.Println(err)
		}
	}
	writer.Flush()

	fmt.Println("Total peers:", peerNum)
	fmt.Println("Connectable peers:", connectablePeerNum)
	fmt.Println("Unable to connect peers:", unconnectablePeerNum)
}

func getLatency(u *url.URL) (time.Duration, error) {
	port := u.Port()
	if port == "" {
		return 0, errors.New("port is empty")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		ips, err := net.LookupIP(u.Hostname())
		if err != nil {
			return 0, err
		}
		ip = ips[0]
	}

	var remoteAddr string
	if ip.To16() == nil {
		remoteAddr = ip.String() + ":" + port
	} else {
		remoteAddr = "[" + ip.String() + "]:" + port
	}
	timeout := time.Duration(dialTimeout) * time.Second

	t := time.Now()
	switch u.Scheme {
	case "tcp", "ws":
		conn, err := net.DialTimeout("tcp", remoteAddr, timeout)
		if err != nil {
			return 0, err
		}
		err = conn.Close()
		return time.Since(t), err
	case "tls", "wss":
		conn, err := net.DialTimeout("tcp", remoteAddr, timeout)
		if err != nil {
			return 0, err
		}
		tlsConn := tls.Client(conn, &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         u.Hostname(),
		})
		err = tlsConn.Close()
		return time.Since(t), err
	case "quic":
		conn, err := quic.DialAddr(context.Background(), remoteAddr, &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         u.Hostname(),
		}, &quic.Config{
			HandshakeIdleTimeout: timeout,
		})
		if err != nil {
			return 0, err
		}
		stream, err := conn.OpenStreamSync(context.Background())
		if err != nil {
			return 0, err
		}
		err = stream.Close()
		return time.Since(t), err
	}
	return 0, errors.New("invalid protocol")
}
