/**
 * SecurePortal renders the public-facing Confidential Send page
 * mounted at `/secure/:token`. It is intentionally unauthenticated
 * — token (URL) + optional password are the only credentials. The
 * BFF rate-limits attempts via Valkey (5 per token per 15 min).
 */

import { useCallback, useEffect, useState } from "react";
import { useParams } from "react-router-dom";

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
        <h1>Secure portal</h1>
        <p>No token in URL.</p>
      </main>
    );
  }

  if (loading && !message && !needsPassword) {
    return (
      <main className="kmail-secure-portal">
        <h1>Secure portal</h1>
        <p>Loading…</p>
      </main>
    );
  }

  if (needsPassword && !message) {
    return (
      <main className="kmail-secure-portal">
        <h1>Secure portal</h1>
        <p>This message is password-protected.</p>
        <form onSubmit={onSubmit}>
          <label>
            Password
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
            />
          </label>
          <button type="submit" disabled={loading || !password}>
            {loading ? "Checking…" : "Open message"}
          </button>
        </form>
        {error && <p className="kmail-error">{error}</p>}
        <p className="kmail-portal-hint">
          The link is rate-limited to 5 attempts every 15 minutes per token.
        </p>
      </main>
    );
  }

  if (error) {
    return (
      <main className="kmail-secure-portal">
        <h1>Secure portal</h1>
        <p className="kmail-error">{error}</p>
      </main>
    );
  }

  if (!message) return null;

  const remainingViews =
    message.max_views > 0
      ? Math.max(0, message.max_views - message.view_count)
      : null;

  return (
    <main className="kmail-secure-portal">
      <h1>Confidential message</h1>
      <p className="kmail-portal-meta">
        From: <code>{message.sender_id}</code>
        <br />
        Expires: <code>{message.expires_at}</code>
        <br />
        Views: {message.view_count}
        {remainingViews !== null && <> &middot; Remaining: {remainingViews}</>}
      </p>
      <article className="kmail-portal-body">
        <p>
          The encrypted message envelope lives in zk-object-fabric under
          reference: <code>{message.encrypted_blob_ref ?? "—"}</code>
        </p>
        <p>
          The KMail BFF only stores opaque pointers; the actual ciphertext is
          fetched and decrypted client-side.
        </p>
      </article>
    </main>
  );
}
