package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ipfs/boxo/files"
	unixfs "github.com/ipfs/boxo/ipld/unixfs"
	unixfs_pb "github.com/ipfs/boxo/ipld/unixfs/pb"
	"github.com/ipfs/boxo/path"
	"github.com/ipfs/go-cid"
	iface "github.com/ipfs/kubo/core/coreiface"
	caopts "github.com/ipfs/kubo/core/coreiface/options"
	mh "github.com/multiformats/go-multihash"
)

type addEvent struct {
	Name  string
	Hash  string `json:",omitempty"`
	Bytes int64  `json:",omitempty"`
	Size  string `json:",omitempty"`
}

type UnixfsAPI HttpApi

func (api *UnixfsAPI) Add(ctx context.Context, f files.Node, opts ...caopts.UnixfsAddOption) (path.ImmutablePath, error) {
	options, _, err := caopts.UnixfsAddOptions(opts...)
	if err != nil {
		return path.ImmutablePath{}, err
	}

	mht, ok := mh.Codes[options.MhType]
	if !ok {
		return path.ImmutablePath{}, fmt.Errorf("unknowm mhType %d", options.MhType)
	}

	req := api.core().Request("add").
		Option("hash", mht).
		Option("chunker", options.Chunker).
		Option("cid-version", options.CidVersion).
		Option("fscache", options.FsCache).
		Option("inline", options.Inline).
		Option("inline-limit", options.InlineLimit).
		Option("nocopy", options.NoCopy).
		Option("only-hash", options.OnlyHash).
		Option("pin", options.Pin).
		Option("silent", options.Silent).
		Option("progress", options.Progress)

	if options.RawLeavesSet {
		req.Option("raw-leaves", options.RawLeaves)
	}

	switch options.Layout {
	case caopts.BalancedLayout:
		// noop, default
	case caopts.TrickleLayout:
		req.Option("trickle", true)
	}

	d := files.NewMapDirectory(map[string]files.Node{"": f}) // unwrapped on the other side

	version, err := api.core().loadRemoteVersion()
	if err != nil {
		return path.ImmutablePath{}, err
	}
	useEncodedAbsPaths := version.LT(encodedAbsolutePathVersion)
	req.Body(files.NewMultiFileReader(d, false, useEncodedAbsPaths))

	var out addEvent
	resp, err := req.Send(ctx)
	if err != nil {
		return path.ImmutablePath{}, err
	}
	if resp.Error != nil {
		return path.ImmutablePath{}, resp.Error
	}
	defer resp.Output.Close()
	dec := json.NewDecoder(resp.Output)

	for {
		var evt addEvent
		if err := dec.Decode(&evt); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return path.ImmutablePath{}, err
		}
		out = evt

		if options.Events != nil {
			ifevt := &iface.AddEvent{
				Name:  out.Name,
				Size:  out.Size,
				Bytes: out.Bytes,
			}

			if out.Hash != "" {
				c, err := cid.Parse(out.Hash)
				if err != nil {
					return path.ImmutablePath{}, err
				}

				ifevt.Path = path.FromCid(c)
			}

			select {
			case options.Events <- ifevt:
			case <-ctx.Done():
				return path.ImmutablePath{}, ctx.Err()
			}
		}
	}

	c, err := cid.Parse(out.Hash)
	if err != nil {
		return path.ImmutablePath{}, err
	}

	return path.FromCid(c), nil
}

type lsLink struct {
	Name, Hash string
	Size       uint64
	Type       unixfs_pb.Data_DataType
	Target     string

	Mode    os.FileMode
	ModTime time.Time
}

type lsObject struct {
	Hash  string
	Links []lsLink
}

type lsOutput struct {
	Objects []lsObject
}

func (api *UnixfsAPI) Ls(ctx context.Context, p path.Path, out chan<- iface.DirEntry, opts ...caopts.UnixfsLsOption) error {
	defer close(out)

	options, err := caopts.UnixfsLsOptions(opts...)
	if err != nil {
		return err
	}

	resp, err := api.core().Request("ls", p.String()).
		Option("resolve-type", options.ResolveChildren).
		Option("size", options.ResolveChildren).
		Option("stream", true).
		Send(ctx)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return err
	}
	defer resp.Close()

	dec := json.NewDecoder(resp.Output)

	for {
		var link lsOutput
		if err = dec.Decode(&link); err != nil {
			if err != io.EOF {
				return err
			}
			return nil
		}

		if len(link.Objects) != 1 {
			return errors.New("unexpected Objects len")
		}

		if len(link.Objects[0].Links) != 1 {
			return errors.New("unexpected Links len")
		}

		l0 := link.Objects[0].Links[0]

		c, err := cid.Decode(l0.Hash)
		if err != nil {
			return err
		}

		var ftype iface.FileType
		switch l0.Type {
		case unixfs.TRaw, unixfs.TFile:
			ftype = iface.TFile
		case unixfs.THAMTShard, unixfs.TDirectory, unixfs.TMetadata:
			ftype = iface.TDirectory
		case unixfs.TSymlink:
			ftype = iface.TSymlink
		}

		select {
		case out <- iface.DirEntry{
			Name:   l0.Name,
			Cid:    c,
			Size:   l0.Size,
			Type:   ftype,
			Target: l0.Target,

			Mode:    l0.Mode,
			ModTime: l0.ModTime,
		}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (api *UnixfsAPI) core() *HttpApi {
	return (*HttpApi)(api)
}
