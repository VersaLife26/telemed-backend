-- Security review F20(d): stop rendering the patient's free-text cancellation
-- reason into a notification body.
--
-- events.AppointmentCancelled.Reason is up to 500 characters written by the
-- patient. The appointment_cancelled templates interpolated it, which put that
-- text into notifications.body, into notification_dead_letters when a send
-- failed, and -- on the sms channel -- into Dialog's or Twilio's message log.
-- On a medical cancellation, free text of that length routinely contains
-- clinical detail.
--
-- internal/notification/consumer.go no longer passes Reason for this template.
-- Because text/template runs with Option("missingkey=zero"), leaving the
-- placeholders here would not have errored -- it would have rendered
-- "...has been cancelled. " and "<p>Reason: </p>", which is worse than either
-- alternative: a visible artefact in a patient-facing message, and a template
-- that still LOOKS like it carries the reason to the next person who reads it.
--
-- Only the six rows that actually referenced {{.Reason}} change: the three sms
-- bodies and the three email bodies. push and in_app never carried it.
--
-- doctor_rejected and payment_failed keep their {{.Reason}}: one is an
-- admin-authored credentialing decision sent to the doctor it is about, the
-- other is a payment gateway's failure code. Neither is patient-authored
-- clinical text, and in both cases the reason is the entire point of the
-- message.

UPDATE templates SET body_template =
    'Your appointment with Dr. {{.DoctorName}} on {{.DateTime}} has been cancelled.'
WHERE key = 'appointment_cancelled' AND channel = 'sms' AND locale = 'en';

UPDATE templates SET body_template =
    'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව ({{.DateTime}}) අවලංගු කර ඇත.'
WHERE key = 'appointment_cancelled' AND channel = 'sms' AND locale = 'si';

UPDATE templates SET body_template =
    'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு ({{.DateTime}}) ரத்து செய்யப்பட்டது.'
WHERE key = 'appointment_cancelled' AND channel = 'sms' AND locale = 'ta';

-- The email bodies keep a second paragraph, pointing at the appointment record
-- -- which is where the reason legitimately lives, behind authentication --
-- rather than silently dropping the sentence.
UPDATE templates SET body_template =
    '<p>Your appointment with Dr. {{.DoctorName}} on {{.DateTime}} has been cancelled.</p>'
    '<p>Open the appointment in the app for the full details.</p>'
WHERE key = 'appointment_cancelled' AND channel = 'email' AND locale = 'en';

UPDATE templates SET body_template =
    '<p>ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව {{.DateTime}} සඳහා අවලංගු කර ඇත.</p>'
    '<p>සම්පූර්ණ විස්තර සඳහා යෙදුමේ හමුව විවෘත කරන්න.</p>'
WHERE key = 'appointment_cancelled' AND channel = 'email' AND locale = 'si';

UPDATE templates SET body_template =
    '<p>மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு {{.DateTime}} அன்று ரத்து செய்யப்பட்டது.</p>'
    '<p>முழு விவரங்களுக்கு செயலியில் சந்திப்பைத் திறக்கவும்.</p>'
WHERE key = 'appointment_cancelled' AND channel = 'email' AND locale = 'ta';
