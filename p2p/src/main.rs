//! `covibe-p2p` — a deliberately dumb byte pump between unix sockets and iroh QUIC streams.
//!
//! The sidecar knows nothing about covibe's terminal protocol. It moves opaque bytes,
//! preserving order and boundaries-agnostic content, in exactly the directions it is told to.
//!
//! Two subcommands:
//!
//! * `host --rw-sock <path> --ro-sock <path> [--relay <url>]` binds two independent iroh
//!   endpoints (distinct ephemeral keypairs — the ticket *is* the capability), prints one
//!   JSON line `{"rw":"…","ro":"…"}` to stdout and then bridges every incoming connection
//!   to a fresh unix-socket connection. On the `ro` endpoint the peer→socket direction is
//!   explicitly torn down: peer bytes can never reach the socket.
//! * `dial --ticket <ticket>` connects to a ticket and pumps stdin→stream, stream→stdout.
//!
//! stdout is sacred: in `host` it carries the single JSON line and nothing else, in `dial`
//! it carries stream bytes and nothing else. Everything diagnostic goes to stderr.

use std::{path::PathBuf, str::FromStr, sync::Arc, time::Duration};

use anyhow::{Context, Result, bail};
use iroh::{
    Endpoint, EndpointAddr, RelayMap, RelayMode, RelayUrl, SecretKey, Watcher,
    endpoint::{Connection, RecvStream, SendStream, VarInt, presets},
};
use iroh_tickets::{Ticket, endpoint::EndpointTicket};
use tokio::{
    io::{AsyncRead, AsyncWrite, AsyncWriteExt},
    net::{
        UnixStream,
        unix::{OwnedReadHalf, OwnedWriteHalf},
    },
};

/// The application-layer protocol negotiated on every covibe connection.
const ALPN: &[u8] = b"covibe/term/1";

/// How long to wait for the endpoint to report a relay address before giving up.
///
/// A ticket without a relay URL stops working the moment the host roams or its NAT
/// mapping is rebound, so we treat a missing relay as a hard failure rather than
/// silently emitting a degraded ticket.
const RELAY_WAIT: Duration = Duration::from_secs(30);

/// Error code used when we tear down a stream direction we will never read.
const CODE_READ_ONLY: u32 = 1;

/// How long to let the peer drain and close after both copy directions are done.
///
/// `Connection::close` discards un-acknowledged stream data, so the last bytes we
/// wrote would be lost if we closed the moment the copies returned. The dialer closes
/// once it has consumed the stream; this is only a backstop for a peer that never does.
const DRAIN_GRACE: Duration = Duration::from_secs(30);

fn main() -> Result<()> {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .context("building tokio runtime")?;
    rt.block_on(run())
}

async fn run() -> Result<()> {
    match Command::parse(std::env::args().skip(1))? {
        Command::Host {
            rw_sock,
            ro_sock,
            relay,
        } => host(rw_sock, ro_sock, relay).await,
        Command::Dial { ticket } => dial(ticket).await,
    }
}

// --- argument parsing -------------------------------------------------------------------

const USAGE: &str = "\
usage:
  covibe-p2p host --rw-sock <path> --ro-sock <path> [--relay <url>]
  covibe-p2p dial --ticket <ticket>";

enum Command {
    Host {
        rw_sock: PathBuf,
        ro_sock: PathBuf,
        relay: Option<RelayUrl>,
    },
    Dial {
        ticket: EndpointTicket,
    },
}

impl Command {
    fn parse(args: impl IntoIterator<Item = String>) -> Result<Self> {
        let mut args = args.into_iter();
        let sub = args.next().unwrap_or_default();

        let mut rw_sock = None;
        let mut ro_sock = None;
        let mut relay = None;
        let mut ticket = None;

        while let Some(flag) = args.next() {
            let mut value = || {
                args.next()
                    .with_context(|| format!("flag {flag} requires a value"))
            };
            match flag.as_str() {
                "--rw-sock" => rw_sock = Some(PathBuf::from(value()?)),
                "--ro-sock" => ro_sock = Some(PathBuf::from(value()?)),
                "--relay" => {
                    let raw = value()?;
                    relay = Some(
                        RelayUrl::from_str(&raw)
                            .with_context(|| format!("invalid relay url {raw:?}"))?,
                    );
                }
                "--ticket" => {
                    let raw = value()?;
                    ticket = Some(
                        EndpointTicket::from_str(&raw)
                            .map_err(|err| anyhow::anyhow!("invalid ticket: {err}"))?,
                    );
                }
                other => bail!("unknown flag {other:?}\n{USAGE}"),
            }
        }

        match sub.as_str() {
            "host" => Ok(Command::Host {
                rw_sock: rw_sock.context("host requires --rw-sock <path>")?,
                ro_sock: ro_sock.context("host requires --ro-sock <path>")?,
                relay,
            }),
            "dial" => Ok(Command::Dial {
                ticket: ticket.context("dial requires --ticket <ticket>")?,
            }),
            "" => bail!("missing subcommand\n{USAGE}"),
            other => bail!("unknown subcommand {other:?}\n{USAGE}"),
        }
    }
}

// --- host -------------------------------------------------------------------------------

/// Which direction of the bridge is allowed to carry peer bytes.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Access {
    /// Bytes flow both ways.
    ReadWrite,
    /// Only socket→peer. Peer→socket is torn down at the QUIC layer.
    ReadOnly,
}

impl Access {
    fn label(self) -> &'static str {
        match self {
            Access::ReadWrite => "rw",
            Access::ReadOnly => "ro",
        }
    }
}

async fn host(rw_sock: PathBuf, ro_sock: PathBuf, relay: Option<RelayUrl>) -> Result<()> {
    let relay_mode = match relay {
        Some(url) => RelayMode::Custom(RelayMap::from(url)),
        None => RelayMode::Default,
    };

    // Two endpoints, two freshly generated keypairs. The ticket is the capability, so the
    // read-only and read-write sides must not share an identity.
    let (rw_ep, ro_ep) = tokio::try_join!(
        bind_endpoint(Access::ReadWrite, relay_mode.clone()),
        bind_endpoint(Access::ReadOnly, relay_mode),
    )?;

    let (rw_addr, ro_addr) = tokio::try_join!(
        addr_with_relay(&rw_ep, Access::ReadWrite),
        addr_with_relay(&ro_ep, Access::ReadOnly),
    )?;

    let rw_ticket = EndpointTicket::new(rw_addr).encode_string();
    let ro_ticket = EndpointTicket::new(ro_addr).encode_string();

    // The Go parent blocks on this line. It is the only thing we ever write to stdout,
    // and it must be flushed before we start serving.
    {
        use std::io::Write;
        let stdout = std::io::stdout();
        let mut stdout = stdout.lock();
        writeln!(stdout, r#"{{"rw":"{rw_ticket}","ro":"{ro_ticket}"}}"#)
            .context("writing ticket line")?;
        stdout.flush().context("flushing ticket line")?;
    }

    let rw = tokio::spawn(serve(rw_ep, Arc::from(rw_sock), Access::ReadWrite));
    let ro = tokio::spawn(serve(ro_ep, Arc::from(ro_sock), Access::ReadOnly));
    let (rw, ro) = tokio::try_join!(rw, ro).context("serve task panicked")?;
    rw?;
    ro?;
    Ok(())
}

async fn bind_endpoint(access: Access, relay_mode: RelayMode) -> Result<Endpoint> {
    Endpoint::builder(presets::N0)
        .secret_key(SecretKey::generate())
        .alpns(vec![ALPN.to_vec()])
        .relay_mode(relay_mode)
        .bind()
        .await
        .with_context(|| format!("binding {} endpoint", access.label()))
}

/// Waits until the endpoint has a relay address, then snapshots its [`EndpointAddr`].
async fn addr_with_relay(endpoint: &Endpoint, access: Access) -> Result<EndpointAddr> {
    let wait = async {
        let mut watcher = endpoint.watch_addr();
        loop {
            let addr = watcher.get();
            if addr.relay_urls().next().is_some() {
                return addr;
            }
            if watcher.updated().await.is_err() {
                // The watcher only disconnects when the endpoint is gone; nothing left to do.
                std::future::pending::<()>().await;
            }
        }
    };
    match tokio::time::timeout(RELAY_WAIT, wait).await {
        Ok(addr) => {
            let relays: Vec<_> = addr.relay_urls().map(RelayUrl::to_string).collect();
            eprintln!(
                "covibe-p2p: {} endpoint {} online via {}",
                access.label(),
                addr.id,
                relays.join(", ")
            );
            Ok(addr)
        }
        Err(_) => bail!(
            "{} endpoint did not obtain a relay address within {}s",
            access.label(),
            RELAY_WAIT.as_secs()
        ),
    }
}

async fn serve(endpoint: Endpoint, sock: Arc<std::path::Path>, access: Access) -> Result<()> {
    while let Some(incoming) = endpoint.accept().await {
        let sock = Arc::clone(&sock);
        tokio::spawn(async move {
            match incoming.await {
                Ok(conn) => {
                    let peer = conn.remote_id();
                    match bridge(&conn, &sock, access).await {
                        Ok(()) => {
                            // Let the peer finish reading and close first, so nothing
                            // still in flight is discarded by our own close.
                            let _ = tokio::time::timeout(DRAIN_GRACE, conn.closed()).await;
                        }
                        Err(err) => {
                            eprintln!("covibe-p2p: {} bridge to {peer}: {err:#}", access.label())
                        }
                    }
                    conn.close(0u32.into(), b"bye");
                }
                Err(err) => eprintln!("covibe-p2p: {} handshake failed: {err}", access.label()),
            }
        });
    }
    Ok(())
}

/// Bridges one accepted connection to one fresh unix-socket connection.
async fn bridge(conn: &Connection, sock: &std::path::Path, access: Access) -> Result<()> {
    let (send, recv) = conn
        .accept_bi()
        .await
        .context("accepting bidirectional stream")?;
    let unix = UnixStream::connect(sock)
        .await
        .with_context(|| format!("connecting unix socket {}", sock.display()))?;
    let (unix_read, unix_write) = unix.into_split();

    match access {
        Access::ReadWrite => pump_rw(send, recv, unix_read, unix_write).await,
        Access::ReadOnly => pump_ro(send, recv, unix_read, unix_write).await,
    }
}

/// Read-write: copy in both directions until either side closes.
async fn pump_rw(
    send: SendStream,
    recv: RecvStream,
    unix_read: OwnedReadHalf,
    unix_write: OwnedWriteHalf,
) -> Result<()> {
    let to_socket = tokio::spawn(copy_then_close(recv, unix_write));
    let to_peer = tokio::spawn(copy_then_close(unix_read, send));
    let (a, b) = tokio::try_join!(to_socket, to_peer).context("bridge task panicked")?;
    a.context("peer -> socket")?;
    b.context("socket -> peer")?;
    Ok(())
}

/// Read-only: peer bytes are never written to the socket.
///
/// We do not merely ignore the peer's data — we tear the direction down at the QUIC layer
/// with `STOP_SENDING` and shut the socket's write half, so there is no code path and no
/// buffer through which a viewer's keystrokes could reach the session.
async fn pump_ro(
    send: SendStream,
    recv: RecvStream,
    unix_read: OwnedReadHalf,
    mut unix_write: OwnedWriteHalf,
) -> Result<()> {
    let mut recv = recv;
    let _ = recv.stop(VarInt::from_u32(CODE_READ_ONLY));
    drop(recv);
    let _ = unix_write.shutdown().await;
    drop(unix_write);

    copy_then_close(unix_read, send)
        .await
        .context("socket -> peer")?;
    Ok(())
}

/// Copies `from` into `to` until EOF, then closes the write side of `to`.
async fn copy_then_close<R, W>(mut from: R, mut to: W) -> Result<u64>
where
    R: AsyncRead + Unpin,
    W: AsyncWrite + Unpin,
{
    let copied = tokio::io::copy(&mut from, &mut to).await;
    // Signal EOF downstream regardless of how the copy ended, so the far side unblocks.
    let _ = to.shutdown().await;
    Ok(copied?)
}

// --- dial -------------------------------------------------------------------------------

async fn dial(ticket: EndpointTicket) -> Result<()> {
    let addr = EndpointAddr::from(ticket);
    let endpoint = Endpoint::builder(presets::N0)
        .bind()
        .await
        .context("binding dial endpoint")?;

    let conn = endpoint
        .connect(addr, ALPN)
        .await
        .context("connecting to ticket")?;
    eprintln!("covibe-p2p: connected to {}", conn.remote_id());

    // A QUIC stream does not exist for the peer until the side that opened it writes,
    // so covibe's client sends its snapshot request immediately on attach. Without
    // that first message the host would sit in accept_bi and the terminal would stay
    // blank until the user happened to press a key.
    let (send, recv) = conn.open_bi().await.context("opening stream")?;

    // stdin -> stream runs detached, and its failure is deliberately silent. A
    // read-only host answers our first write with STOP_SENDING, which is expected
    // rather than exceptional; any other write failure means the connection is gone,
    // and the stream -> stdout direction below reports that once instead of twice.
    let up = tokio::spawn(async move {
        let _ = copy_then_close(tokio::io::stdin(), send).await;
    });

    match copy_then_close(recv, tokio::io::stdout()).await {
        Ok(copied) => eprintln!("covibe-p2p: stream closed after {copied} bytes"),
        // A closed or lost connection is how an attach normally ends: the session was
        // killed, or the far side went away. That is not a failure of this process, so
        // it must not exit non-zero and frighten the caller.
        Err(err) if conn.close_reason().is_some() => {
            eprintln!("covibe-p2p: disconnected ({err:#})");
        }
        Err(err) => return Err(err).context("stream -> stdout"),
    }

    up.abort();
    conn.close(0u32.into(), b"bye");
    endpoint.close().await;
    Ok(())
}
