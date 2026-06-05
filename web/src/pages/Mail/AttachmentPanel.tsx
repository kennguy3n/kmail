import { useState } from "react";

import { jmapClient } from "../../api/jmap";
import type { EmailBodyPart } from "../../types";
import { formatBytes } from "./messageContent";

/**
 * Attachment list with inline preview and download.
 *
 * Renders the file attachments of a message (inline cid: images are
 * excluded by the caller). Each row downloads its blob on demand via
 * {@link jmapClient.downloadBlob} — the JMAP download endpoint needs
 * an auth header, so we fetch to a Blob and hand the browser an
 * object URL rather than linking the raw URL. Images and PDFs can be
 * previewed in place; "Download all" saves every attachment.
 */
export default function AttachmentPanel({
  attachments,
}: {
  attachments: EmailBodyPart[];
}) {
  const [previewId, setPreviewId] = useState<string | null>(null);
  const [previewUrl, setPreviewUrl] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (attachments.length === 0) return null;

  const keyFor = (a: EmailBodyPart, i: number) =>
    a.blobId ?? a.partId ?? a.name ?? `att-${i}`;

  const saveBlob = (blob: Blob, name: string) => {
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = name;
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
    // Defer revoke so the navigation/download has the URL.
    setTimeout(() => URL.revokeObjectURL(url), 10_000);
  };

  const download = async (a: EmailBodyPart) => {
    if (!a.blobId) return;
    setError(null);
    try {
      const blob = await jmapClient.downloadBlob(
        a.blobId,
        a.name ?? "attachment",
        a.type ?? "application/octet-stream",
      );
      saveBlob(blob, a.name ?? "attachment");
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  const downloadAll = async () => {
    setBusy(true);
    setError(null);
    try {
      for (const a of attachments) {
        await download(a);
      }
    } finally {
      setBusy(false);
    }
  };

  const togglePreview = async (a: EmailBodyPart, key: string) => {
    if (previewId === key) {
      setPreviewId(null);
      if (previewUrl) URL.revokeObjectURL(previewUrl);
      setPreviewUrl(null);
      return;
    }
    if (!a.blobId) return;
    setError(null);
    try {
      const blob = await jmapClient.downloadBlob(
        a.blobId,
        a.name ?? "attachment",
        a.type ?? "application/octet-stream",
      );
      if (previewUrl) URL.revokeObjectURL(previewUrl);
      setPreviewUrl(URL.createObjectURL(blob));
      setPreviewId(key);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  const canPreview = (type: string | null | undefined) =>
    !!type && (type.startsWith("image/") || type === "application/pdf");

  return (
    <section style={styles.box}>
      <div style={styles.headerRow}>
        <h2 style={styles.title}>Attachments ({attachments.length})</h2>
        {attachments.length > 1 && (
          <button
            type="button"
            onClick={() => void downloadAll()}
            disabled={busy}
            style={styles.downloadAll}
          >
            {busy ? "Downloading…" : "Download all"}
          </button>
        )}
      </div>
      {error && <div style={styles.error}>{error}</div>}
      <ul style={styles.list}>
        {attachments.map((a, i) => {
          const key = keyFor(a, i);
          const previewable = canPreview(a.type);
          return (
            <li key={key} style={styles.item}>
              <div style={styles.itemRow}>
                <span style={styles.fileIcon} aria-hidden="true">
                  📎
                </span>
                <span style={styles.name}>{a.name ?? "(unnamed)"}</span>
                <span style={styles.meta}>
                  {a.type ?? "application/octet-stream"}
                  {typeof a.size === "number"
                    ? ` · ${formatBytes(a.size)}`
                    : ""}
                </span>
                <span style={styles.spacer} />
                {previewable && (
                  <button
                    type="button"
                    onClick={() => void togglePreview(a, key)}
                    style={styles.smallButton}
                  >
                    {previewId === key ? "Hide" : "Preview"}
                  </button>
                )}
                <button
                  type="button"
                  onClick={() => void download(a)}
                  style={styles.smallButton}
                >
                  Download
                </button>
              </div>
              {previewId === key && previewUrl && (
                <div style={styles.preview}>
                  {a.type?.startsWith("image/") ? (
                    <img
                      src={previewUrl}
                      alt={a.name ?? "attachment preview"}
                      style={styles.previewImage}
                    />
                  ) : (
                    <iframe
                      title={`Preview of ${a.name ?? "attachment"}`}
                      src={previewUrl}
                      style={styles.previewFrame}
                    />
                  )}
                </div>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}

const styles: Record<string, React.CSSProperties> = {
  box: { marginTop: "1rem", paddingTop: "0.75rem", borderTop: "1px solid #e5e7eb" },
  headerRow: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    marginBottom: "0.5rem",
  },
  title: { margin: 0, fontSize: "0.95rem" },
  downloadAll: {
    padding: "0.3rem 0.6rem",
    fontSize: "0.8rem",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    color: "#374151",
  },
  list: { listStyle: "none", margin: 0, padding: 0, display: "grid", gap: "0.4rem" },
  item: {
    border: "1px solid #e5e7eb",
    borderRadius: "0.375rem",
    padding: "0.4rem 0.5rem",
    background: "#fff",
  },
  itemRow: {
    display: "flex",
    alignItems: "center",
    gap: "0.5rem",
    fontSize: "0.85rem",
  },
  fileIcon: { fontSize: "0.9rem" },
  name: { fontWeight: 600, color: "#111827" },
  meta: { color: "#6b7280", fontSize: "0.78rem" },
  spacer: { flex: 1 },
  smallButton: {
    padding: "0.25rem 0.55rem",
    fontSize: "0.78rem",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    color: "#374151",
  },
  preview: {
    marginTop: "0.5rem",
    borderTop: "1px solid #f3f4f6",
    paddingTop: "0.5rem",
  },
  previewImage: {
    maxWidth: "100%",
    maxHeight: "480px",
    borderRadius: "0.25rem",
  },
  previewFrame: {
    width: "100%",
    height: "520px",
    border: "1px solid #e5e7eb",
    borderRadius: "0.25rem",
  },
  error: {
    padding: "0.4rem 0.6rem",
    background: "#fee2e2",
    color: "#991b1b",
    borderRadius: "0.25rem",
    fontSize: "0.82rem",
    marginBottom: "0.5rem",
  },
};
