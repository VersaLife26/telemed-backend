-- Restores the {{.Reason}} placeholders 000003 seeded. Rolling this back
-- re-opens F20(d) only if consumer.go is also reverted -- with the current
-- code the placeholder simply renders empty -- but "reverse the up migration"
-- has to mean the rows go back to what they were.

UPDATE templates SET body_template =
    'Your appointment with Dr. {{.DoctorName}} on {{.DateTime}} has been cancelled. {{.Reason}}'
WHERE key = 'appointment_cancelled' AND channel = 'sms' AND locale = 'en';

UPDATE templates SET body_template =
    'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව ({{.DateTime}}) අවලංගු කර ඇත. {{.Reason}}'
WHERE key = 'appointment_cancelled' AND channel = 'sms' AND locale = 'si';

UPDATE templates SET body_template =
    'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு ({{.DateTime}}) ரத்து செய்யப்பட்டது. {{.Reason}}'
WHERE key = 'appointment_cancelled' AND channel = 'sms' AND locale = 'ta';

UPDATE templates SET body_template =
    '<p>Your appointment with Dr. {{.DoctorName}} on {{.DateTime}} has been cancelled.</p><p>Reason: {{.Reason}}</p>'
WHERE key = 'appointment_cancelled' AND channel = 'email' AND locale = 'en';

UPDATE templates SET body_template =
    '<p>ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව {{.DateTime}} සඳහා අවලංගු කර ඇත.</p><p>හේතුව: {{.Reason}}</p>'
WHERE key = 'appointment_cancelled' AND channel = 'email' AND locale = 'si';

UPDATE templates SET body_template =
    '<p>மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு {{.DateTime}} அன்று ரத்து செய்யப்பட்டது.</p><p>காரணம்: {{.Reason}}</p>'
WHERE key = 'appointment_cancelled' AND channel = 'email' AND locale = 'ta';
