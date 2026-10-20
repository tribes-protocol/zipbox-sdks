// Quickstart: create (or reuse) a box, run a command, write and read a file,
// open a preview, stop the box.
//
//	export ZIPBOX_API_KEY=zb_…
//	go run .
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	zipbox "github.com/tribes-protocol/zipbox-go"
)

func main() {
	ctx := context.Background()
	c := zipbox.New("")

	box, err := c.Sandboxes.GetOrCreate(ctx, "quickstart")
	if err != nil {
		log.Fatal(err)
	}

	res, err := box.Exec(ctx, "echo hello from $(hostname)")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(res.Stdout)

	if err := box.Files.Write(ctx, "hello.txt", strings.NewReader("hi\n")); err != nil {
		log.Fatal(err)
	}
	data, err := box.Files.Read(ctx, "hello.txt")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("hello.txt: %s", data)

	if _, err := box.ExecBackground(ctx, "python3 -m http.server 8000"); err != nil {
		log.Fatal(err)
	}
	preview, err := box.Preview.Create(ctx, 8000, "web")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("preview:", preview.URL)

	if err := c.Sandboxes.Stop(ctx, box.ID); err != nil {
		log.Fatal(err)
	}
	fmt.Println("stopped", box.ID)
}
