package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/Sirupsen/logrus"
	"github.com/boltdb/bolt"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
)

var volumesBucket = []byte("volumes")

type gateway struct {
	root string
	db   *bolt.DB
	mu   sync.Mutex
}

type nfsExport struct {
	Path    string
	Hosts   []string
	Options string
}

type volume struct {
	Name   string
	Export nfsExport
}

type CreateRequest struct {
	Hosts   []string
	Options string
}

type CreateResponse struct {
	Name string
	Path string
}

func (g *gateway) createVolume(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "must supply a name parameter", http.StatusBadRequest)
		return
	}

	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, errors.Wrap(err, "error decoding request").Error(), http.StatusBadRequest)
		return
	}

	var v *volume
	err := g.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(volumesBucket)
		data := b.Get([]byte(name))
		if data != nil {
			return nil
		}

		v = &volume{
			Name: name,
			Export: nfsExport{
				Hosts:   req.Hosts,
				Path:    g.nfsPath(name),
				Options: req.Options,
			},
		}

		vb, err := json.Marshal(v)
		if err != nil {
			return errors.Wrap(err, "error marshaling volume data")
		}
		if err := b.Put([]byte(name), vb); err != nil {
			return errors.Wrap(err, "error writing volume to database")
		}

		if err := os.MkdirAll(v.Export.Path, 0755); err != nil {
			return errors.Wrap(err, "error creating volume dir")
		}

		if err := exportfs(v); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if v == nil {
		http.Error(w, "already exists", http.StatusConflict)
		return
	}

	resp := CreateResponse{
		Name: v.Name,
		Path: v.Export.Path,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, errors.Wrap(err, "error marshaling response").Error(), http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

func (g *gateway) nfsPath(name string) string {
	return filepath.Join(g.root, "nfs", name)
}

type GetResponse struct {
	Name string
	Path string
}

func (g *gateway) getVolume(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := mux.Vars(r)["name"]
	if name == "" {
		http.Error(w, "name parameter must be set", http.StatusBadRequest)
		return
	}

	var vol *volume
	err := g.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(volumesBucket).Get([]byte(name))
		if data == nil {
			return nil
		}
		var v volume
		if err := json.Unmarshal(data, &v); err != nil {
			return errors.Wrap(err, "error unmarshaling volume data from database")
		}
		vol = &v
		return nil
	})
	if err != nil {
		http.Error(w, errors.Wrap(err, "error reading from database").Error(), http.StatusInternalServerError)
		return
	}

	if vol == nil {
		http.Error(w, "volume not found", http.StatusNotFound)
		return
	}

	resp := GetResponse{
		Name: vol.Name,
		Path: vol.Export.Path,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, errors.Wrap(err, "error marshaling response").Error(), http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

func (g *gateway) deleteVolume(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := mux.Vars(r)["name"]
	if name == "" {
		http.Error(w, "must provide name parameter", http.StatusBadRequest)
		return
	}

	err := g.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(volumesBucket)
		data := b.Get([]byte(name))
		if data == nil {
			return nil
		}

		if err := b.Delete([]byte(name)); err != nil {
			errors.Wrap(err, "error deleting entry from the database")
		}

		var v volume
		if err := json.Unmarshal(data, &v); err != nil {
			return errors.Wrap(err, "error unmarshaling volume from database")
		}

		if err := unexport(&v); err != nil {
			return err
		}
		if err := os.RemoveAll(v.Export.Path); err != nil && !os.IsNotExist(err) {
			return errors.Wrap(err, "error removing volume data")
		}
		return nil
	})

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func exportfs(v *volume) error {
	var args []string
	for _, h := range v.Export.Hosts {
		if v.Export.Options != "" {
			args = append(args, "-o", v.Export.Options)
		}
		args = append(args, h+":"+v.Export.Path)
	}
	return errors.Wrap(cmd(exportfsPath, args...), "error making nfs export")
}

func (*gateway) Shutdown() {
	err := cmd(exportfsPath, "-ua")
	if err != nil {
		logrus.WithError(err).Error("error during shutdown")
	}
}

func (g *gateway) Reload() error {
	return g.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(volumesBucket)
		return b.ForEach(func(k []byte, v []byte) error {
			var vol *volume
			if err := json.Unmarshal(v, &vol); err != nil {
				return errors.Wrap(err, "error unmarshaling volume from database")
			}

			if err := exportfs(vol); err != nil {
				logrus.WithError(err).WithField("volume", vol.Name).Error("error exporting volume on reload")
			}
			return nil
		})
	})
}

func unexport(v *volume) error {
	args := []string{"-u"}
	for _, h := range v.Export.Hosts {
		args = append(args, h+":"+v.Export.Path)
	}
	return errors.Wrap(cmd(exportfsPath, args...), "error unexporting nfs dir")
}

func cmd(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	return errors.Wrap(err, string(out))
}
