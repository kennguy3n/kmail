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
    <section className={styles.box}>
      <div className={styles.headerRow}>
        <h2 className={styles.title}>Attachments ({attachments.length})</h2>
        {attachments.length > 1 && (
          <button
            type="button"
            onClick={() => void downloadAll()}
            disabled={busy}
            className={styles.downloadAll}
          >
            {busy ? "Downloading…" : "Download all"}
          </button>
        )}
      </div>
      {error && <div className={styles.error}>{error}</div>}
      <ul className={styles.list}>
        {attachments.map((a, i) => {
          const key = keyFor(a, i);
          const previewable = canPreview(a.type);
          return (
            <li key={key} className={styles.item}>
              <div className={styles.itemRow}>
                <span className={styles.fileIcon} aria-hidden="true">
                  📎
                </span>
                <span className={styles.name}>{a.name ?? "(unnamed)"}</span>
                <span className={styles.meta}>
                  {a.type ?? "application/octet-stream"}
                  {typeof a.size === "number"
                    ? ` · ${formatBytes(a.size)}`
                    : ""}
                </span>
                <span className={styles.spacer} />
                {previewable && (
                  <button
                    type="button"
                    onClick={() => void togglePreview(a, key)}
                    className={styles.smallButton}
                  >
                    {previewId === key ? "Hide" : "Preview"}
                  </button>
                )}
                <button
                  type="button"
                  onClick={() => void download(a)}
                  className={styles.smallButton}
                >
                  Download
                </button>
              </div>
              {previewId === key && previewUrl && (
                <div className={styles.preview}>
                  {a.type?.startsWith("image/") ? (
                    <img
                      src={previewUrl}
                      alt={a.name ?? "attachment preview"}
                      className={styles.previewImage}
                    />
                  ) : (
                    <iframe
                      title={`Preview of ${a.name ?? "attachment"}`}
                      src={previewUrl}
                      className={styles.previewFrame}
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

/** Theme-aware Tailwind class recipes for the AttachmentPanel. */
const styles: Record<string, string> = {
  box: "mt-4 border-t border-border pt-3",
  headerRow: "mb-2 flex items-center justify-between",
  title: "m-0 text-sm font-semibold",
  downloadAll:
    "cursor-pointer rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs text-fg transition-colors hover:bg-surface-hover",
  list: "m-0 grid list-none gap-1.5 p-0",
  item: "rounded-md border border-border bg-surface px-2 py-1.5",
  itemRow: "flex items-center gap-2 text-sm",
  fileIcon: "text-sm",
  name: "font-semibold text-fg",
  meta: "text-xs text-fg-muted",
  spacer: "flex-1",
  smallButton:
    "cursor-pointer rounded-md border border-border bg-surface px-2 py-1 text-xs text-fg transition-colors hover:bg-surface-hover",
  preview: "mt-2 border-t border-border pt-2",
  previewImage: "max-h-[480px] max-w-full rounded-sm",
  previewFrame: "h-[520px] w-full rounded-sm border border-border",
  error: "mb-2 rounded-md bg-danger-bg px-2.5 py-1.5 text-xs text-danger-fg",
};
