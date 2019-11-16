// Copyright (c) Microsoft. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for details.
package main

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"fmt"
	"golang.org/x/net/context"
	"os"
	"os/user"
	"path"
	"strings"
	"sync"
	"time"
)

// Encapsulates state and operations for directory node on the HDFS file system
type Dir struct {
	FileSystem   *FileSystem        // Pointer to the owning filesystem
	Attrs        Attrs              // Cached attributes of the directory, TODO: add TTL
	Parent       *Dir               // Pointer to the parent directory (allows computing fully-qualified paths on demand)
	Entries      map[string]fs.Node // Cahed directory entries
	EntriesMutex sync.Mutex         // Used to protect Entries
}

// Verify that *Dir implements necesary FUSE interfaces
var _ fs.Node = (*Dir)(nil)
var _ fs.HandleReadDirAller = (*Dir)(nil)
var _ fs.NodeStringLookuper = (*Dir)(nil)
var _ fs.NodeMkdirer = (*Dir)(nil)
var _ fs.NodeRemover = (*Dir)(nil)
var _ fs.NodeRenamer = (*Dir)(nil)

// Returns absolute path of the dir in HDFS namespace
func (this *Dir) AbsolutePath() string {
	if this.Parent == nil {
		return "/"
	} else {
		return path.Join(this.Parent.AbsolutePath(), this.Attrs.Name)
	}
}

// Returns absolute path of the child item of this directory
func (this *Dir) AbsolutePathForChild(name string) string {
	path := this.AbsolutePath()
	if path != "/" {
		path = path + "/"
	}
	return path + name
}

// Responds on FUSE request to get directory attributes
func (this *Dir) Attr(ctx context.Context, a *fuse.Attr) error {
	if this.Parent != nil && this.FileSystem.Clock.Now().After(this.Attrs.Expires) {
		err := this.Parent.LookupAttrs(this.Attrs.Name, &this.Attrs)
		if err != nil {
			return err
		}

	}
	return this.Attrs.Attr(a)
}

func (this *Dir) EntriesGet(name string) fs.Node {
	this.EntriesMutex.Lock()
	defer this.EntriesMutex.Unlock()
	if this.Entries == nil {
		this.Entries = make(map[string]fs.Node)
		return nil
	}
	return this.Entries[name]
}

func (this *Dir) EntriesSet(name string, node fs.Node) {
	this.EntriesMutex.Lock()
	defer this.EntriesMutex.Unlock()

	if this.Entries == nil {
		this.Entries = make(map[string]fs.Node)
	}

	this.Entries[name] = node
}

func (this *Dir) EntriesRemove(name string) {
	this.EntriesMutex.Lock()
	defer this.EntriesMutex.Unlock()
	if this.Entries != nil {
		delete(this.Entries, name)
	}
}

// Responds on FUSE request to lookup the directory
func (this *Dir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if !this.FileSystem.IsPathAllowed(this.AbsolutePathForChild(name)) {
		return nil, fuse.ENOENT
	}

	if node := this.EntriesGet(name); node != nil {
		return node, nil
	}

	if this.FileSystem.ExpandZips && strings.HasSuffix(name, ".zip@") {
		// looking up original zip file
		zipFileName := name[:len(name)-1]
		zipFileNode, err := this.Lookup(nil, zipFileName)
		if err != nil {
			return nil, err
		}
		zipFile, ok := zipFileNode.(*File)
		if !ok {
			return nil, fuse.ENOENT
		}
		attrs := zipFile.Attrs
		attrs.Mode |= os.ModeDir | 0111 // TODO: set x only if r is set
		attrs.Name = name
		attrs.Inode = 0 // let underlying FUSE layer to assign inodes automatically
		return NewZipRootDir(zipFile, attrs), nil
	}

	var attrs Attrs
	err := this.LookupAttrs(name, &attrs)
	if err != nil {
		return nil, err
	}
	return this.NodeFromAttrs(attrs), nil
}

// Responds on FUSE request to read directory
func (this *Dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	absolutePath := this.AbsolutePath()
	Info.Println("[", absolutePath, "]ReadDirAll")

	allAttrs, err := this.FileSystem.HdfsAccessor.ReadDir(absolutePath)
	if err != nil {
		Warning.Println("ls [", absolutePath, "]: ", err)
		return nil, err
	}
	entries := make([]fuse.Dirent, 0, len(allAttrs))
	for _, a := range allAttrs {
		if this.FileSystem.IsPathAllowed(this.AbsolutePathForChild(a.Name)) {
			// Creating Dirent structure as required by FUSE
			entries = append(entries, fuse.Dirent{
				Inode: a.Inode,
				Name:  a.Name,
				Type:  a.FuseNodeType()})
			// Speculatively pre-creating child Dir or File node with cached attributes,
			// since it's highly likely that we will have Lookup() call for this name
			// This is the key trick which dramatically speeds up 'ls'
			this.NodeFromAttrs(a)

			if this.FileSystem.ExpandZips {
				// Creating a virtual directory next to each zip file
				// (appending '@' to the zip file name)
				if !a.Mode.IsDir() && strings.HasSuffix(a.Name, ".zip") {
					entries = append(entries, fuse.Dirent{
						Name: a.Name + "@",
						Type: fuse.DT_Dir})
				}
			}
		}
	}
	return entries, nil
}

// Creates typed node (Dir or File) from the attributes
func (this *Dir) NodeFromAttrs(attrs Attrs) fs.Node {
	var node fs.Node
	if (attrs.Mode & os.ModeDir) == 0 {
		node = &File{FileSystem: this.FileSystem, Parent: this, Attrs: attrs}
	} else {
		node = &Dir{FileSystem: this.FileSystem, Parent: this, Attrs: attrs}
	}
	this.EntriesSet(attrs.Name, node)
	return node
}

// Performs Stat() query on the backend
func (this *Dir) LookupAttrs(name string, attrs *Attrs) error {
	var err error
	*attrs, err = this.FileSystem.HdfsAccessor.Stat(path.Join(this.AbsolutePath(), name))
	if err != nil {
		// It is a warning as each time new file write tries to stat if the file exists
		Warning.Print("stat [", name, "]: ", err.Error(), err)
		if pathError, ok := err.(*os.PathError); ok && (pathError.Err == os.ErrNotExist) {
			return fuse.ENOENT
		}
		return err
	}
	// expiration time := now + 5 secs // TODO: make configurable
	attrs.Expires = this.FileSystem.Clock.Now().Add(5 * time.Second)
	return nil
}

// Responds on FUSE Mkdir request
func (this *Dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	err := this.FileSystem.HdfsAccessor.Mkdir(this.AbsolutePathForChild(req.Name), req.Mode)
	if err != nil {
		return nil, err
	}
	return this.NodeFromAttrs(Attrs{Name: req.Name, Mode: req.Mode | os.ModeDir}), nil
}

// Responds on FUSE Create request
func (this *Dir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	Info.Println("[", this.AbsolutePathForChild(req.Name), "] Create ", req.Mode)
	file := this.NodeFromAttrs(Attrs{Name: req.Name, Mode: req.Mode}).(*File)
	handle := NewFileHandle(file)
	err := handle.EnableWrite(true)
	if err != nil {
		Error.Println("Can't create file: ", this.AbsolutePathForChild(req.Name), err)
		return nil, nil, err
	}
	file.AddHandle(handle)
	return file, handle, nil
}

// Responds on FUSE Remove request
func (this *Dir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	path := this.AbsolutePathForChild(req.Name)
	Info.Println("Remove", path)
	err := this.FileSystem.HdfsAccessor.Remove(path)
	if err == nil {
		this.EntriesRemove(req.Name)
	}
	return err
}

// Responds on FUSE Rename request
func (this *Dir) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node, server *fs.Server) error {
	oldPath := this.AbsolutePathForChild(req.OldName)
	newPath := newDir.(*Dir).AbsolutePathForChild(req.NewName)
	Info.Println("Rename [", oldPath, "] to ", newPath)
	err := this.FileSystem.HdfsAccessor.Rename(oldPath, newPath)
	var snode fs.Node
	if err == nil {
		// Upon successful rename, updating in-memory representation of the file entry
		if node := this.EntriesGet(req.OldName); node != nil {
			if fnode, ok := node.(*File); ok {
				snode = server.GetNode(fnode.Attrs.Inode)
			} else if dnode, ok := node.(*Dir); ok {
				snode = server.GetNode(dnode.Attrs.Inode)
			}
			if snode == nil {
				return err
			}
			if sfnode, ok := snode.(*File); ok {
				sfnode.Attrs.Name = req.NewName
				sfnode.Parent = newDir.(*Dir)
			} else if sdnode, ok := snode.(*Dir); ok {
				sdnode.Attrs.Name = req.NewName
				sdnode.Parent = newDir.(*Dir)
			}
			this.EntriesRemove(req.OldName)
			newDir.(*Dir).EntriesSet(req.NewName, snode)
		}
	}
	return err
}

// Responds on FUSE Chmod request
func (this *Dir) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	// Get the filepath, so chmod in hdfs can work
	path := this.AbsolutePath()
	var err error

	if req.Valid.Mode() {
		Info.Println("Chmod [", path, "] to [", req.Mode, "]")
		(func() {
			err = this.FileSystem.HdfsAccessor.Chmod(path, req.Mode)
			if err != nil {
				return
			}
		})()

		if err != nil {
			Error.Println("Chmod [", path, "] failed with error: ", err)
		} else {
			this.Attrs.Mode = req.Mode
		}
	}

	if req.Valid.Uid() {
		u, err := user.LookupId(fmt.Sprint(req.Uid))
		owner := fmt.Sprint(req.Uid)
		group := fmt.Sprint(req.Gid)
		if err != nil {
			Error.Println("Chown: username for uid", req.Uid, "not found, use uid/gid instead")
		} else {
			owner = u.Username
			group = owner // hardcoded the group same as owner until LookupGroupId available
		}

		Info.Println("Chown [", path, "] to [", owner, ":", group, "]")
		(func() {
			err = this.FileSystem.HdfsAccessor.Chown(path, owner, group)
			if err != nil {
				return
			}
		})()

		if err != nil {
			Error.Println("Chown failed with error:", err)
		} else {
			this.Attrs.Uid = req.Uid
			this.Attrs.Gid = req.Gid
		}
	}

	return err
}
