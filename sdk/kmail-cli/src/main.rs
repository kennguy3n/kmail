// `kmail` debug CLI.
//
// Single binary that drives `kmail_core::KMailClient` against a
// running BFF without needing an iOS / Android / Electron shell.
// Used by:
//
//   * Engineers verifying a new BFF deploy ("can the SDK talk to
//     it?") without spinning up an Electron build.
//   * The nightly integration test workflow that boots the kmail
//     docker compose stack and runs `kmail sync` against it.
//   * Support staff debugging a customer report — they ship the
//     SQLite database from the customer's device and the CLI
//     re-opens it locally.
//
// Subcommands:
//
//   kmail session  --bff <url> --token <oidc>
//   kmail sync     --bff <url> --token <oidc> --db <path>
//   kmail mailboxes --db <path>
//   kmail emails   --db <path> --mailbox <id> [--limit N]
//   kmail email    --bff <url> --token <oidc> --db <path> --id <id> [--with-bodies]
//   kmail doctor   --db <path>   # prints SQLite + schema info
//
// The CLI is intentionally NOT a thin wrapper around an HTTP
// debugging tool — it goes through the SDK's actual API surface
// so it doubles as smoke coverage for KMailClient itself.

use std::path::PathBuf;

use anyhow::{Context, Result};
use clap::{Parser, Subcommand};
use kmail_core::{ClientConfig, KMailClient};

#[derive(Parser, Debug)]
#[command(name = "kmail", version, about = "KMail SDK debug CLI", long_about = None)]
struct Cli {
    #[command(subcommand)]
    command: Cmd,
}

#[derive(Subcommand, Debug)]
enum Cmd {
    /// Fetch the JMAP session resource from the BFF.
    Session(SessionArgs),
    /// Run a single delta-pull sync.
    Sync(SyncArgs),
    /// List mailboxes already cached in the local SQLite store.
    Mailboxes(LocalDbArgs),
    /// List the most recent emails in a cached mailbox.
    Emails(EmailsArgs),
    /// Fetch a single email by ID (network round-trip).
    Email(EmailArgs),
    /// Show local store version + JMAP state tokens.
    Doctor(LocalDbArgs),
}

#[derive(Parser, Debug)]
struct SessionArgs {
    /// Absolute BFF base URL.
    #[arg(long)]
    bff: String,
    /// OIDC bearer token.
    #[arg(long, env = "KMAIL_TOKEN")]
    token: String,
}

#[derive(Parser, Debug)]
struct SyncArgs {
    #[arg(long)]
    bff: String,
    #[arg(long, env = "KMAIL_TOKEN")]
    token: String,
    #[arg(long)]
    db: PathBuf,
    #[arg(long, default_value_t = 200)]
    initial_window: u32,
}

#[derive(Parser, Debug)]
struct LocalDbArgs {
    #[arg(long)]
    db: PathBuf,
}

#[derive(Parser, Debug)]
struct EmailsArgs {
    #[arg(long)]
    db: PathBuf,
    #[arg(long)]
    mailbox: String,
    #[arg(long, default_value_t = 50)]
    limit: u32,
}

#[derive(Parser, Debug)]
struct EmailArgs {
    #[arg(long)]
    bff: String,
    #[arg(long, env = "KMAIL_TOKEN")]
    token: String,
    #[arg(long)]
    db: PathBuf,
    #[arg(long)]
    id: String,
    #[arg(long)]
    with_bodies: bool,
}

#[tokio::main]
async fn main() -> Result<()> {
    init_tracing();
    let cli = Cli::parse();
    match cli.command {
        Cmd::Session(a) => run_session(a).await,
        Cmd::Sync(a) => run_sync(a).await,
        Cmd::Mailboxes(a) => run_mailboxes(a),
        Cmd::Emails(a) => run_emails(a),
        Cmd::Email(a) => run_email(a).await,
        Cmd::Doctor(a) => run_doctor(a),
    }
}

fn init_tracing() {
    let env_filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("kmail_cli=info,kmail_core=info"));
    tracing_subscriber::fmt()
        .with_env_filter(env_filter)
        .with_target(false)
        .init();
}

fn open_client(bff: &str, token: &str, db: &std::path::Path) -> Result<KMailClient> {
    let cfg = ClientConfig::new(bff, token, db.to_path_buf());
    KMailClient::open(cfg).context("failed to open KMailClient")
}

fn open_client_local(db: &std::path::Path) -> Result<KMailClient> {
    // For local-only commands we still need *some* BFF URL because
    // KMailClient::open validates the field is non-empty. Use a
    // safe sentinel — no network calls happen on these commands.
    open_client("https://localhost.invalid", "local-only", db)
}

async fn run_session(args: SessionArgs) -> Result<()> {
    let dir = tempfile::tempdir().context("failed to create temp dir for session probe")?;
    let db = dir.path().join("session-probe.db");
    let client = open_client(&args.bff, &args.token, &db)?;
    let session = client
        .discover_session()
        .await
        .context("session discovery failed")?;
    println!("{}", serde_json::to_string_pretty(&session)?);
    Ok(())
}

async fn run_sync(args: SyncArgs) -> Result<()> {
    let mut cfg = ClientConfig::new(args.bff, args.token, args.db);
    cfg.initial_sync_email_window = args.initial_window;
    let client = KMailClient::open(cfg).context("open client failed")?;
    let summary = client.sync().await.context("sync failed")?;
    println!(
        "{}",
        serde_json::to_string_pretty(&serde_json::json!({
            "mailboxesUpserted": summary.mailboxes_upserted,
            "emailsCreated": summary.emails_created,
            "emailsUpdated": summary.emails_updated,
            "emailsDestroyed": summary.emails_destroyed,
            "pendingActionsFlushed": summary.pending_actions_flushed,
        }))?
    );
    Ok(())
}

fn run_mailboxes(args: LocalDbArgs) -> Result<()> {
    let client = open_client_local(&args.db)?;
    let mailboxes = client.cached_mailboxes().context("read mailboxes failed")?;
    let json: Vec<_> = mailboxes
        .iter()
        .map(|m| {
            serde_json::json!({
                "id": m.id,
                "name": m.name,
                "role": m.role.map(|r| r.canonical_name()),
                "total": m.total_emails,
                "unread": m.unread_emails,
                "vault": m.is_vault,
            })
        })
        .collect();
    println!("{}", serde_json::to_string_pretty(&json)?);
    Ok(())
}

fn run_emails(args: EmailsArgs) -> Result<()> {
    let client = open_client_local(&args.db)?;
    let emails = client
        .cached_emails_in_mailbox(&args.mailbox, args.limit)
        .context("read emails failed")?;
    let json: Vec<_> = emails
        .iter()
        .map(|e| {
            serde_json::json!({
                "id": e.id,
                "threadId": e.thread_id,
                "subject": e.subject,
                "from": e.from.iter().map(|a| &a.email).collect::<Vec<_>>(),
                "receivedAt": e.received_at.to_rfc3339(),
                "unread": !e.keywords.get("$seen").copied().unwrap_or(false),
                "hasAttachment": e.has_attachment,
            })
        })
        .collect();
    println!("{}", serde_json::to_string_pretty(&json)?);
    Ok(())
}

async fn run_email(args: EmailArgs) -> Result<()> {
    let client = open_client(&args.bff, &args.token, &args.db)?;
    let email = client
        .fetch_email(&args.id, args.with_bodies)
        .await
        .context("fetch email failed")?;
    println!("{}", serde_json::to_string_pretty(&email)?);
    Ok(())
}

fn run_doctor(args: LocalDbArgs) -> Result<()> {
    let client = open_client_local(&args.db)?;
    let store = client.store();
    let version: i64 = store
        .with_conn(|c| {
            Ok(
                c.query_row("SELECT MAX(version) FROM schema_version", [], |r| {
                    r.get::<_, i64>(0)
                })?,
            )
        })
        .map_err(|e| anyhow::anyhow!("schema probe failed: {e}"))?;
    let mailboxes_n: i64 = store
        .with_conn(|c| {
            Ok(c.query_row("SELECT COUNT(*) FROM mailboxes", [], |r| r.get::<_, i64>(0))?)
        })
        .map_err(|e| anyhow::anyhow!("mailbox count failed: {e}"))?;
    let emails_n: i64 = store
        .with_conn(|c| Ok(c.query_row("SELECT COUNT(*) FROM emails", [], |r| r.get::<_, i64>(0))?))
        .map_err(|e| anyhow::anyhow!("email count failed: {e}"))?;
    let pending_n: i64 = store
        .with_conn(|c| {
            Ok(
                c.query_row("SELECT COUNT(*) FROM pending_actions", [], |r| {
                    r.get::<_, i64>(0)
                })?,
            )
        })
        .map_err(|e| anyhow::anyhow!("pending count failed: {e}"))?;
    println!(
        "{}",
        serde_json::to_string_pretty(&serde_json::json!({
            "sqliteVersion": kmail_core::sync::Store::sqlite_version(),
            "schemaVersion": version,
            "mailboxes": mailboxes_n,
            "emails": emails_n,
            "pendingActions": pending_n,
            "databasePath": args.db.display().to_string(),
        }))?
    );
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// `Cli::parse_from` accepts every subcommand declared above.
    /// Regressions here usually mean a `#[command(subcommand)]`
    /// arm was renamed in clap-parsed flags without the test being
    /// updated to match.
    #[test]
    fn cli_accepts_every_subcommand() {
        Cli::parse_from(["kmail", "session", "--bff", "http://x", "--token", "t"]);
        Cli::parse_from([
            "kmail", "sync", "--bff", "http://x", "--token", "t", "--db", "/tmp/x",
        ]);
        Cli::parse_from(["kmail", "mailboxes", "--db", "/tmp/x"]);
        Cli::parse_from(["kmail", "emails", "--db", "/tmp/x", "--mailbox", "mbx-1"]);
        Cli::parse_from([
            "kmail", "email", "--bff", "http://x", "--token", "t", "--db", "/tmp/x", "--id", "e1",
        ]);
        Cli::parse_from(["kmail", "doctor", "--db", "/tmp/x"]);
    }
}
