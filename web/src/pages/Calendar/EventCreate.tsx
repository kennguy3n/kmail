/**
 * EventCreate is the event creation form.
 *
 * In Phase 2 it will drive `CalendarEvent/set` through the BFF
 * and, where the KChat integration is on, emit a chat message
 * into the meeting's backing channel via the Calendar Bridge. In
 * Phase 1 it is a placeholder.
 */
export default function EventCreate() {
  return (
    <section>
      <h2>New event</h2>
      <p>Not yet implemented.</p>
    </section>
  );
}
