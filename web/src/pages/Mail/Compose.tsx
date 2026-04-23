/**
 * Compose is the message composition view.
 *
 * In Phase 2 it will drive `Email/set` + `EmailSubmission/set`
 * through the BFF, enforce Confidential-Send plan gating
 * (docs/JMAP-CONTRACT.md §4.2), and upload attachments via
 * `Blob/upload` into the zk-object-fabric-backed blob store. In
 * Phase 1 it is a placeholder.
 */
export default function Compose() {
  return (
    <section>
      <h2>Compose</h2>
      <p>Not yet implemented.</p>
    </section>
  );
}
