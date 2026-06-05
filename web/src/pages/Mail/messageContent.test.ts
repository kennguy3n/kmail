/**
 * Unit tests for the message-content helpers: attachment
 * classification, byte formatting, and cid reference rewriting.
 */
import { describe, expect, it } from "vitest";

import {
  fileAttachments,
  formatBytes,
  inlineImageParts,
  resolveCidReferences,
} from "./messageContent";
import type { Email, EmailBodyPart } from "../../types";

function part(overrides: Partial<EmailBodyPart>): EmailBodyPart {
  return {
    partId: null,
    blobId: "blob1",
    size: 0,
    name: null,
    type: "application/octet-stream",
    charset: null,
    disposition: null,
    cid: null,
    language: null,
    location: null,
    subParts: null,
    ...overrides,
  };
}

function email(attachments: EmailBodyPart[]): Email {
  return {
    id: "e1",
    blobId: "b",
    threadId: "t1",
    mailboxIds: {},
    keywords: {},
    size: 0,
    receivedAt: "2026-01-01T00:00:00Z",
    from: null,
    to: null,
    cc: null,
    bcc: null,
    replyTo: null,
    subject: null,
    sentAt: null,
    attachments,
  } as Email;
}

describe("inlineImageParts / fileAttachments", () => {
  it("classifies cid image parts as inline and the rest as files", () => {
    const inlineImg = part({
      blobId: "img",
      cid: "logo@x",
      type: "image/png",
      disposition: "inline",
    });
    const pdf = part({ blobId: "doc", name: "report.pdf", type: "application/pdf" });
    const e = email([inlineImg, pdf]);

    expect(inlineImageParts(e).map((p) => p.blobId)).toEqual(["img"]);
    expect(fileAttachments(e).map((p) => p.blobId)).toEqual(["doc"]);
  });

  it("does not treat a cid-less image as inline", () => {
    const e = email([part({ blobId: "img", type: "image/png", cid: null })]);
    expect(inlineImageParts(e)).toHaveLength(0);
    expect(fileAttachments(e)).toHaveLength(1);
  });
});

describe("formatBytes", () => {
  it("formats bytes, KB and MB", () => {
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(2048)).toBe("2.0 KB");
    expect(formatBytes(5 * 1024 * 1024)).toBe("5.0 MB");
  });

  it("returns an empty string for missing sizes", () => {
    expect(formatBytes(null)).toBe("");
    expect(formatBytes(undefined)).toBe("");
  });
});

describe("resolveCidReferences", () => {
  it("rewrites cid: references that have a resolved url", () => {
    const html = '<img src="cid:Logo@X"> <img src="cid:missing">';
    const out = resolveCidReferences(html, { "logo@x": "blob:abc" });
    expect(out).toContain('src="blob:abc"');
    expect(out).toContain('src="cid:missing"');
  });
});
