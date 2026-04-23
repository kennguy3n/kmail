/**
 * CalendarView is the personal / team calendar view.
 *
 * In Phase 2 it will render events from the JMAP calendars
 * capability (`urn:ietf:params:jmap:calendars`) and surface RSVP
 * state. Team and resource calendars are read-only until the
 * caller has the right ACL in `calendar_metadata` (docs/SCHEMA.md
 * §5.9). In Phase 1 it is a placeholder.
 */
export default function CalendarView() {
  return (
    <section>
      <h2>Calendar</h2>
      <p>Not yet implemented.</p>
    </section>
  );
}
