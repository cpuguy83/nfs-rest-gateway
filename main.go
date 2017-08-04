package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/boltdb/bolt"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

var exportfsPath string

func main() {
	flListenAddr := flag.String("H", "127.0.0.1:80", "address to listen on")
	flDataRoot := flag.String("root", "/var/lib/nfsg", "location to store data")
	flag.Parse()

	var err error
	exportfsPath, err = exec.LookPath("exportfs")
	exitOnError(err, "could not find required binary 'exportfs'")

	err = os.MkdirAll(filepath.Join(*flDataRoot, "nfs"), 0755)
	exitOnError(err, "error making data root")

	db, err := bolt.Open(filepath.Join(*flDataRoot, "volumes.db"), 0600, &bolt.Options{
		Timeout: 10 * time.Second,
	})
	exitOnError(err, "error setting up boltdb")
	defer db.Close()

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(volumesBucket)
		return err
	})
	exitOnError(err, "error creating volume bucket in database")

	err = setupNFS()
	exitOnError(err, "error preparing NFS")

	g := &gateway{root: *flDataRoot, db: db}
	go handleShutdown(g)
	err = g.Reload()
	exitOnError(err, "error on reload")

	l, err := net.Listen("tcp", *flListenAddr)
	exitOnError(err, "error setting up TCP listener")
	defer l.Close()

	router := makeRouter(g)
	http.Serve(l, router)
}

func makeRouter(g *gateway) *mux.Router {
	r := mux.NewRouter()
	r.Methods("POST").Path("/volume").HandlerFunc(g.createVolume)
	r.Methods("GET").Path("/volume/{name}").HandlerFunc(g.getVolume)
	r.Methods("DELETE").Path("/volume/{name}").HandlerFunc(g.deleteVolume)
	return r
}

func exitOnError(err error, message string) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, message+":", err.Error())
	os.Exit(1)
}

func setupNFS() error {
	fsData, err := ioutil.ReadFile("/proc/filesystems")
	if err == nil {
		if !bytes.Contains(fsData, []byte("nfsd")) {
			// best effort
			cmd("modprobe", "-q", "nfsd")
		}
	}

	// mount -t nfsd -o nodev,noexec,nosuid nfsd /proc/fs/nfsd
	if err := unix.Mount("nfsd", "/proc/fs/nfsd", "nfsd", unix.MS_NOEXEC|unix.MS_NODEV|unix.MS_NOSUID, ""); err != nil && err != unix.EBUSY {
		return errors.Wrap(err, "error mounting nfsd")
	}
	for _, dir := range []string{"rpc_pipefs", "v4recovery", "v4root"} {
		if err := os.MkdirAll(filepath.Join("/var", "lib", "nfs", dir), 0755); err != nil {
			return errors.Wrap(err, "error setting up nfs dirs")
		}
	}

	cmd := exec.Command("/usr/sbin/rpc.mountd")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGTERM,
	}
	if err := cmd.Start(); err == nil {
		go cmd.Wait()
	}

	cmd = exec.Command("/usr/sbin/rpc.nfsd")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGTERM,
	}
	if err = cmd.Start(); err == nil {
		go cmd.Wait()
	}

	cmd = exec.Command("/usr/bin/sm-notify")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGTERM,
	}
	if err = cmd.Start(); err == nil {
		go cmd.Wait()
	}

	return nil
}

func handleShutdown(g *gateway) {
	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)

	for range ch {
		g.Shutdown()
		os.Exit(0)
	}
}
