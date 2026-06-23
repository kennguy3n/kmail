import { useEffect, useRef, useState } from "react";

import { jmapClient } from "../../api/jmap";
import type { Email, EmailBodyPart } from "../../types";

/**
 * Shared helpers for rendering an Email's body and attachments,
 * used by both {@link MessageView} and the conversation
 * {@link ThreadView} so the two render messages identically.
 *
 * The HTML body is rendered inside a sandboxed iframe whose sandbox
 * grants `allow-same-origin` but deliberately NOT `allow-scripts`, so
 * scripts embedded in the message never execute while the parent can
 * still read the document to size the frame. Inline `cid:` images are
 * resolved to object URLs by downloading the referenced blobs and
 * rewriting the HTML before it reaches the iframe.
 */

/** The first renderable HTML body part, or null. */
export function htmlBodyPart(email: Email): EmailBodyPart | null {
  const part = email.htmlBody?.find((p) => p.partId);
  if (part?.partId && email.bodyValues?.[part.partId]) return part;
  return null;
}

/** The first plain-text body value, or "". */
export function plainBodyValue(email: Email): string {
  const part = email.textBody?.find((p) => p.partId);
  if (part?.partId && email.bodyValues?.[part.partId]) {
    return email.bodyValues[part.partId].value;
  }
  return "";
}

/**
 * True when a part is an inline image referenced from the HTML body
 * by Content-ID (RFC 2392 `cid:`), as opposed to a file the user
 * should see in the attachments list. Such parts have `cid` set and
 * are either explicitly `inline` or simply carry no filename.
 */
function isInlineImage(part: EmailBodyPart): boolean {
  if (!part.cid || !part.blobId) return false;
  const isImage = (part.type ?? "").startsWith("image/");
  const inlineDisposition = part.disposition
    ? part.disposition.toLowerCase() === "inline"
    : true;
  return isImage && inlineDisposition;
}

/** Inline (cid-referenced) image parts of an email. */
export function inlineImageParts(email: Email): EmailBodyPart[] {
  return (email.attachments ?? []).filter(isInlineImage);
}

/**
 * File attachments to list for the user: everything in
 * `attachments` that isn't an inline image already shown in the
 * body.
 */
export function fileAttachments(email: Email): EmailBodyPart[] {
  return (email.attachments ?? []).filter((p) => !isInlineImage(p));
}

/** Human-readable byte size. */
export function formatBytes(n: number | null | undefined): string {
  if (typeof n !== "number" || Number.isNaN(n)) return "";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

/** Normalise a cid for matching: strip angle brackets and lowercase. */
function normalizeCid(cid: string): string {
  return cid.replace(/^[<]+|[>]+$/g, "").trim().toLowerCase();
}

/**
 * Download every inline image of `email` and return a map from
 * normalized cid → object URL. Object URLs are revoked on cleanup
 * (email change / unmount) so blob references don't leak.
 */
export function useInlineImageUrls(
  email: Email | null,
): Record<string, string> {
  const [urls, setUrls] = useState<Record<string, string>>({});

  useEffect(() => {
    if (!email) {
      setUrls({});
      return;
    }
    const parts = inlineImageParts(email);
    if (parts.length === 0) {
      setUrls({});
      return;
    }
    let cancelled = false;
    const created: string[] = [];
    Promise.all(
      parts.map(async (p) => {
        if (!p.blobId || !p.cid) return null;
        try {
          const blob = await jmapClient.downloadBlob(
            p.blobId,
            p.name ?? "inline",
            p.type ?? "application/octet-stream",
          );
          const url = URL.createObjectURL(blob);
          created.push(url);
          return [normalizeCid(p.cid), url] as const;
        } catch {
          return null;
        }
      }),
    ).then((pairs) => {
      if (cancelled) {
        created.forEach((u) => URL.revokeObjectURL(u));
        return;
      }
      const map: Record<string, string> = {};
      for (const pair of pairs) {
        if (pair) map[pair[0]] = pair[1];
      }
      setUrls(map);
    });
    return () => {
      cancelled = true;
      created.forEach((u) => URL.revokeObjectURL(u));
    };
  }, [email]);

  return urls;
}

/** Replace `cid:` references in HTML with resolved object URLs. */
export function resolveCidReferences(
  html: string,
  cidUrls: Record<string, string>,
): string {
  return html.replace(
    /(["'(])cid:([^"')\s]+)/gi,
    (match, lead: string, cid: string) => {
      const url = cidUrls[normalizeCid(cid)];
      return url ? `${lead}${url}` : match;
    },
  );
}

/**
 * Render an email's HTML body inside a sandboxed iframe and auto-size
 * its height to the content.
 *
 * The sandbox grants `allow-same-origin` so the parent can read the
 * framed document's `scrollHeight`, but withholds `allow-scripts` so
 * scripts embedded in the message never execute — the two MUST NOT be
 * combined here, as that would let untrusted message markup escape the
 * sandbox. Because no script runs inside the frame, height is measured
 * from the parent with a `ResizeObserver`, which also catches late
 * growth as inline `cid:` images (object URLs) finish decoding after
 * the initial load.
 */
export function HtmlMessageBody({
  html,
  cidUrls,
}: {
  html: string;
  cidUrls: Record<string, string>;
}) {
  const [height, setHeight] = useState(120);
  const observerRef = useRef<ResizeObserver | null>(null);
  const resolved = resolveCidReferences(html, cidUrls);
  const srcDoc = `<!doctype html><html><head><meta charset="utf-8">
<base target="_blank">
<style>
  body{margin:0;font-family:Inter,-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;font-size:14px;color:#111827;line-height:1.5;word-wrap:break-word;}
  img{max-width:100%;height:auto;}
  a{color:#4f46e5;}
  blockquote{margin:0 0 0 0.8rem;padding-left:0.8rem;border-left:2px solid #e5e7eb;color:#6b7280;}
</style></head><body>${resolved}</body></html>`;

  useEffect(
    () => () => {
      observerRef.current?.disconnect();
      observerRef.current = null;
    },
    [],
  );

  const measure = (doc: Document) => {
    const h = doc.body?.scrollHeight ?? 0;
    // +8px guards against a scrollbar from sub-pixel rounding.
    if (h > 0) setHeight(h + 8);
  };

  return (
    <iframe
      title="Message content"
      sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"
      srcDoc={srcDoc}
      style={{
        width: "100%",
        border: "none",
        height: `${height}px`,
      }}
      onLoad={(e) => {
        const doc = e.currentTarget.contentDocument;
        if (!doc?.body) return;
        measure(doc);
        observerRef.current?.disconnect();
        const observer = new ResizeObserver(() => measure(doc));
        observer.observe(doc.body);
        observerRef.current = observer;
      }}
    />
  );
}
