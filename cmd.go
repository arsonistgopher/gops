// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/google/gops/internal"
	"github.com/google/gops/signal"
	client "github.com/influxdata/influxdb/client/v2"
)

// A MemStats records statistics about the memory allocator.
type MemStats struct {
	PID          string `json:"pid"`
	Hostname     string `json:"hostname"`
	AppName      string `json:"appname"`
	Alloc        uint64 `json:"alloc"`
	TotalAlloc   uint64 `json:"total-alloc"`
	Sys          uint64 `json:"sys"`
	Lookups      uint64 `json:"lookups"`
	Mallocs      uint64 `json:"mallocs"`
	Frees        uint64 `json:"frees"`
	HeapAlloc    uint64 `json:"heap-alloc"`
	HeapSys      uint64 `json:"heap-sys"`
	HeapIdle     uint64 `json:"heap-idle"`
	HeapInuse    uint64 `json:"heap-in-use"`
	HeapReleased uint64 `json:"heap-released"`
	HeapObjects  uint64 `json:"heap-objects"`
	StackInuse   uint64 `json:"stack-in-use"`
	StackSys     uint64 `json:"stack-sys"`
	MSpanInuse   uint64 `json:"stack-mspan-inuse"`
	MSpanSys     uint64 `json:"stack-mspan-sys"`
	MCacheInuse  uint64 `json:"stack-mcache-inuse"`
	MCacheSys    uint64 `json:"stack-mcache-sys"`
	GCSys        uint64 `json:"gc-sys"`
	OtherSys     uint64 `json:"other-sys"`
}

var cmds = map[string](func(addr net.TCPAddr, params []string) error){
	"stack":         stackTrace,
	"gc":            gc,
	"memstats":      memStats,
	"version":       version,
	"pprof-heap":    pprofHeap,
	"pprof-cpu":     pprofCPU,
	"stats":         stats,
	"trace":         trace,
	"setgc":         setGC,
	"memstatexport": memStatExport,
}

func setGC(addr net.TCPAddr, params []string) error {
	if len(params) != 1 {
		return errors.New("missing gc percentage")
	}
	perc, err := strconv.ParseInt(params[0], 10, strconv.IntSize)
	if err != nil {
		return err
	}
	buf := make([]byte, binary.MaxVarintLen64)
	binary.PutVarint(buf, perc)
	return cmdWithPrint(addr, signal.SetGCPercent, buf...)
}

func stackTrace(addr net.TCPAddr, _ []string) error {
	return cmdWithPrint(addr, signal.StackTrace)
}

func gc(addr net.TCPAddr, _ []string) error {
	_, err := cmd(addr, signal.GC)
	return err
}

func memStats(addr net.TCPAddr, _ []string) error {
	return cmdWithPrint(addr, signal.MemStats)
}

func memStatExport(addr net.TCPAddr, params []string) error {
	return memStatInfluxDBExport(addr, signal.MemStatExport, params)
}

func version(addr net.TCPAddr, _ []string) error {
	return cmdWithPrint(addr, signal.Version)
}

func pprofHeap(addr net.TCPAddr, _ []string) error {
	return pprof(addr, signal.HeapProfile)
}

func pprofCPU(addr net.TCPAddr, _ []string) error {
	fmt.Println("Profiling CPU now, will take 30 secs...")
	return pprof(addr, signal.CPUProfile)
}

func trace(addr net.TCPAddr, _ []string) error {
	fmt.Println("Tracing now, will take 5 secs...")
	out, err := cmd(addr, signal.Trace)
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return errors.New("nothing has traced")
	}
	tmpfile, err := ioutil.TempFile("", "trace")
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(tmpfile.Name(), out, 0); err != nil {
		return err
	}
	fmt.Printf("Trace dump saved to: %s\n", tmpfile.Name())
	// If go tool chain not found, stopping here and keep trace file.
	if _, err := exec.LookPath("go"); err != nil {
		return nil
	}
	defer os.Remove(tmpfile.Name())
	cmd := exec.Command("go", "tool", "trace", tmpfile.Name())
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func pprof(addr net.TCPAddr, p byte) error {

	tmpDumpFile, err := ioutil.TempFile("", "profile")
	if err != nil {
		return err
	}
	{
		out, err := cmd(addr, p)
		if err != nil {
			return err
		}
		if len(out) == 0 {
			return errors.New("failed to read the profile")
		}
		if err := ioutil.WriteFile(tmpDumpFile.Name(), out, 0); err != nil {
			return err
		}
		fmt.Printf("Profile dump saved to: %s\n", tmpDumpFile.Name())
		// If go tool chain not found, stopping here and keep dump file.
		if _, err := exec.LookPath("go"); err != nil {
			return nil
		}
		defer os.Remove(tmpDumpFile.Name())
	}
	// Download running binary
	tmpBinFile, err := ioutil.TempFile("", "binary")
	if err != nil {
		return err
	}
	{
		out, err := cmd(addr, signal.BinaryDump)
		if err != nil {
			return fmt.Errorf("failed to read the binary: %v", err)
		}
		if len(out) == 0 {
			return errors.New("failed to read the binary")
		}
		defer os.Remove(tmpBinFile.Name())
		if err := ioutil.WriteFile(tmpBinFile.Name(), out, 0); err != nil {
			return err
		}
	}
	fmt.Printf("Profiling dump saved to: %s\n", tmpDumpFile.Name())
	fmt.Printf("Binary file saved to: %s\n", tmpBinFile.Name())
	cmd := exec.Command("go", "tool", "pprof", tmpBinFile.Name(), tmpDumpFile.Name())
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func stats(addr net.TCPAddr, _ []string) error {
	return cmdWithPrint(addr, signal.Stats)
}

func cmdWithPrint(addr net.TCPAddr, c byte, params ...byte) error {
	out, err := cmd(addr, c, params...)
	if err != nil {
		return err
	}
	fmt.Printf("%s", out)
	return nil
}

// Return a "JSONified" string. I.e. just append and pre-append {}
func memStatInfluxDBExport(addr net.TCPAddr, c byte, params []string) error {
	// To add more functionality, add params data here and to main file in help const
	// Current params:
	// 		params[0] = host:port string
	// 		params[1] = database

	if len(params) < 2 {
		return errors.New("missing parameters")
	}

	emptyparams := []byte{}

	out, err := cmd(addr, c, emptyparams...)
	if err != nil {
		return err
	}
	jsonout := fmt.Sprintf("{%s}", out)
	fmt.Printf("%s\n", jsonout)

	// Create a new HTTPClient
	ic, err := client.NewHTTPClient(client.HTTPConfig{
		Addr: params[0],
	})
	if err != nil {
		log.Fatal(err)
	}
	defer ic.Close()

	// Create a new point batch
	bp, err := client.NewBatchPoints(client.BatchPointsConfig{
		Database:  params[1],
		Precision: "s",
	})

	if err != nil {
		log.Fatal(err)
	}

	// Create a point and add to batch
	tags := map[string]string{"gops": "gops-values"}

	// Marshall 'jsonout' on to the data structure
	s := MemStats{}

	err = json.Unmarshal([]byte(jsonout), &s)
	if err != nil {
		fmt.Print(err)
	}

	fields := map[string]interface{}{
		"alloc":              uint64(s.Alloc),
		"total-alloc":        uint64(s.TotalAlloc),
		"sys":                uint64(s.Sys),
		"lookups":            uint64(s.Lookups),
		"mallocs":            uint64(s.Mallocs),
		"frees":              uint64(s.Frees),
		"heap-alloc":         uint64(s.HeapAlloc),
		"heap-sys":           uint64(s.HeapSys),
		"heap-idle":          uint64(s.HeapIdle),
		"heap-in-use":        uint64(s.HeapInuse),
		"heap-released":      uint64(s.HeapReleased),
		"heap-objects":       uint64(s.HeapObjects),
		"stack-in-use":       uint64(s.StackInuse),
		"stack-sys":          uint64(s.StackSys),
		"other-sys":          uint64(s.OtherSys),
		"stack-mspan-inuse":  uint64(s.MSpanInuse),
		"stack-mspan-sys":    uint64(s.MSpanSys),
		"stack-mcache-inuse": uint64(s.MCacheInuse),
		"stack-mcache-sys":   uint64(s.MCacheSys),
		"gc-sys":             uint64(s.GCSys),
	}

	// Issue: https://github.com/arsonistgopher/gops/issues/1#issue-352514390 {
	tags["hostname"] = s.Hostname
	tags["pid"] = s.PID
	tags["appname"] = s.AppName

	pt, err := client.NewPoint("gops", tags, fields, time.Now())
	if err != nil {
		fmt.Print(err)
	}
	bp.AddPoint(pt)

	// Write the batch
	if err := ic.Write(bp); err != nil {
		fmt.Print(err)
	}

	// Close client resources
	if err := ic.Close(); err != nil {
		fmt.Print(err)
	}

	return nil
}

// targetToAddr tries to parse the target string, be it remote host:port
// or local process's PID.
func targetToAddr(target string) (*net.TCPAddr, error) {
	if strings.Contains(target, ":") {
		// addr host:port passed
		var err error
		addr, err := net.ResolveTCPAddr("tcp", target)
		if err != nil {
			return nil, fmt.Errorf("couldn't parse dst address: %v", err)
		}
		return addr, nil
	}
	// try to find port by pid then, connect to local
	pid, err := strconv.Atoi(target)
	if err != nil {
		return nil, fmt.Errorf("couldn't parse PID: %v", err)
	}
	port, err := internal.GetPort(pid)
	if err != nil {
		return nil, fmt.Errorf("couldn't get port for PID %v: %v", pid, err)
	}
	addr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:"+port)
	return addr, nil
}

func cmd(addr net.TCPAddr, c byte, params ...byte) ([]byte, error) {
	conn, err := cmdLazy(addr, c, params...)
	if err != nil {
		return nil, fmt.Errorf("couldn't get port by PID: %v", err)
	}

	all, err := ioutil.ReadAll(conn)
	if err != nil {
		return nil, err
	}
	return all, nil
}

func cmdLazy(addr net.TCPAddr, c byte, params ...byte) (io.Reader, error) {
	conn, err := net.DialTCP("tcp", nil, &addr)
	if err != nil {
		return nil, err
	}
	buf := []byte{c}
	buf = append(buf, params...)
	if _, err := conn.Write(buf); err != nil {
		return nil, err
	}
	return conn, nil
}
