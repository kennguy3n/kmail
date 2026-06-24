/**
 * SecurePortal renders the public-facing Confidential Send page
 * mounted at `/secure/:token`. It is intentionally unauthenticated
 * — token (URL) + optional password are the only credentials. The
 * BFF rate-limits attempts via Valkey (5 per token per 15 min).
 */

import { useCallback, useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import { Lock, Mail, Shield, AlertCircle } from "lucide-react";

import { Avatar } from "../../components/ui/Avatar";
import {
  getSecureMessage,
  type SecureMessage,
} from "../../api/confidentialSend";

export default function SecurePortal() {
  const { token } = useParams<{ token: string }>();
  const [message, setMessage] = useState<SecureMessage | null>(null);
  const [needsPassword, setNeedsPassword] = useState(false);
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const probe = useCallback(async () => {
    if (!token) return;
    setLoading(true);
    setError(null);
    try {
      const m = await getSecureMessage(token);
      setMessage(m);
    } catch (err: unknown) {
      const msg = String(err);
      // 401 indicates the link is gated by a password.
      if (msg.includes("401") || msg.toLowerCase().includes("password")) {
        setNeedsPassword(true);
      } else {
        setError(msg);
      }
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    probe();
  }, [probe]);

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!token) return;
    setLoading(true);
    setError(null);
    try {
      const m = await getSecureMessage(token, password);
      setMessage(m);
      setNeedsPassword(false);
    } catch (err: unknown) {
      setError(String(err));
    } finally {
      setLoading(false);
    }
  };

  if (!token) {
    return (
      <main className="kmail-secure-portal">
        <div className="kmail-portal-body text-center">
          <Lock className="mx-auto mb-3 size-10 text-primary" />
          <h1 className="mb-1 text-xl font-semibold">Secure portal</h1>
          <p className="kmail-portal-hint">No token in URL.</p>
        </div>
      </main>
    );
  }

  if (loading && !message && !needsPassword) {
    return (
      <main className="kmail-secure-portal">
        <div className="kmail-portal-body text-center">
          <Lock className="mx-auto mb-3 size-10 text-primary" />
          <h1 className="mb-1 text-xl font-semibold">Secure portal</h1>
          <p className="kmail-portal-hint">Unlocking message…</p>
        </div>
      </main>
    );
  }

  if (needsPassword && !message) {
    return (
      <main className="kmail-secure-portal">
        <div className="kmail-portal-body">
          <div className="mb-4 text-center">
            <div className="mx-auto mb-3 inline-flex size-12 items-center justify-center rounded-full bg-primary-subtle">
              <Lock className="size-6 text-primary" />
            </div>
            <h1 className="mb-1 text-xl font-semibold">This message is protected</h1>
            <p className="kmail-portal-hint">Enter the password shared by the sender to open it.</p>
          </div>
          <form onSubmit={onSubmit} className="flex flex-col gap-3">
            <label className="flex flex-col gap-1 text-sm font-medium text-fg-muted">
              Password
              <input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
                className="rounded-lg border border-border bg-surface px-3 py-2 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle"
              />
            </label>
            <button
              type="submit"
              disabled={loading || !password}
              className="kmail-button"
            >
              {loading ? "Checking…" : "Open message"}
            </button>
          </form>
          {error && <p className="kmail-error">{error}</p>}
          <p className="kmail-portal-hint text-center">
            This link is rate-limited to 5 attempts every 15 minutes per token.
          </p>
        </div>
      </main>
    );
  }

  if (error) {
    return (
      <main className="kmail-secure-portal">
        <div className="kmail-portal-body text-center">
          <AlertCircle className="mx-auto mb-3 size-10 text-danger" />
          <h1 className="mb-1 text-xl font-semibold">Could not open message</h1>
          <p className="kmail-error">{error}</p>
        </div>
      </main>
    );
  }

  if (!message) return null;

  const remainingViews =
    message.max_views > 0
      ? Math.max(0, message.max_views - message.view_count)
      : null;

  const expiry = new Date(message.expires_at).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
  });

  return (
    <main className="kmail-secure-portal">
      <div className="kmail-portal-body">
        <div className="flex items-center justify-center gap-2 rounded-lg bg-primary-subtle px-3 py-2 text-sm font-semibold text-primary">
          <Shield className="size-4" />
          Confidential send — end-to-end encrypted
        </div>

        <div className="flex items-center gap-3 border-b border-border pb-4">
          <Avatar name={message.sender_id} size="lg" />
          <div>
            <div className="font-semibold text-fg">{message.sender_id}</div>
            <div className="text-sm text-fg-muted">Sender</div>
          </div>
        </div>

        <div>
          <h1 className="mb-2 text-xl font-semibold tracking-tight text-fg">
            {message.subject || "Confidential message"}
          </h1>
          <div className="kmail-portal-hint flex flex-wrap gap-3">
            <span className="inline-flex items-center gap-1">
              <Mail className="size-3.5" />
              Expires {expiry}
            </span>
            <span>Viewed {message.view_count} time{message.view_count === 1 ? "" : "s"}</span>
            {remainingViews !== null && <span>{remainingViews} remaining</span>}
          </div>
        </div>

        <article
          className="rounded-lg border border-border bg-surface p-4 text-sm leading-relaxed text-fg"
          dangerouslySetInnerHTML={{
            __html: message.body_html ?? `<p>${message.body ?? "No content."}</p>`,
          }}
        />

        <p className="kmail-portal-hint text-center">
          The encrypted envelope is decrypted client-side. KMail servers cannot read this message.
        </p>
      </div>
    </main>
  );
}
