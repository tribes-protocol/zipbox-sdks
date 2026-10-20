//! `ZIPBOX_API_KEY=zb_… cargo run --example quickstart`
//!
//! Creates (or reuses) a box, runs a command, writes and reads a file, opens a
//! preview, and stops the box.

#[tokio::main(flavor = "current_thread")]
async fn main() -> zipbox::Result<()> {
    let zb = zipbox::Zipbox::from_env()?;
    let sandbox = zb.sandboxes().get_or_create("quickstart").await?;

    let out = sandbox.exec("echo hello from $(hostname)").await?;
    print!("{}", out.stdout);

    sandbox.files().write("hello.txt", "hi\n").await?;
    print!("{}", sandbox.files().read_text("hello.txt").await?);

    sandbox
        .exec_background("python3 -m http.server 8000 --directory /root/workspace")
        .await?;
    let preview = sandbox.preview().create(8000, Some("files")).await?;
    println!("serving at {}", preview.url);

    sandbox.stop().await?;
    Ok(())
}
