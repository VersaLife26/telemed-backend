-- Seed data for templates: 13 template keys x English/Sinhala/Tamil x the
-- channels that make sense for each message (not every key needs every
-- channel -- a payout notice has no reason to be an SMS, an OTP has no
-- business being an email).
--
-- Variable names are consistent across every template in this file:
--   {{.DoctorName}} {{.DateTime}} {{.FeeLKR}} {{.AmountLKR}} {{.Reason}}
--   {{.DownloadURL}} {{.ReceiptURL}} {{.JoinLink}} {{.Code}} {{.ExpiresInMinutes}}
-- sms/push/in_app bodies are parsed with text/template (plain text, no
-- escaping). email subject/body are parsed with html/template, which
-- auto-escapes every interpolated value -- the reason email bodies below are
-- written as HTML fragments rather than plain text.
--
-- Sinhala and Tamil copy below is genuine translation, written in plain,
-- formal register rather than idiom, per the instruction to prefer an
-- unambiguous construction over an invented one. It has NOT been reviewed by
-- a native speaker -- see the build report for the specific strings that
-- most need that review before this goes anywhere near production traffic.

-- =============================================================================
-- booking_confirmed (normal) -- sms, push, email, in_app
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('booking_confirmed', 'sms', 'en', 'normal', NULL,
 'Your appointment with Dr. {{.DoctorName}} is confirmed for {{.DateTime}}. Fee: {{.FeeLKR}}. Thank you for booking.'),
('booking_confirmed', 'sms', 'si', 'normal', NULL,
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව {{.DateTime}} සඳහා තහවුරු කර ඇත. ගාස්තුව: {{.FeeLKR}}. වෙන් කිරීම සඳහා ස්තුතියි.'),
('booking_confirmed', 'sms', 'ta', 'normal', NULL,
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு {{.DateTime}} அன்று உறுதி செய்யப்பட்டுள்ளது. கட்டணம்: {{.FeeLKR}}. முன்பதிவு செய்ததற்கு நன்றி.'),

('booking_confirmed', 'push', 'en', 'normal', 'Appointment confirmed',
 'Your appointment with Dr. {{.DoctorName}} is confirmed for {{.DateTime}}.'),
('booking_confirmed', 'push', 'si', 'normal', 'හමුව තහවුරු කරන ලදී',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව {{.DateTime}} සඳහා තහවුරු කර ඇත.'),
('booking_confirmed', 'push', 'ta', 'normal', 'சந்திப்பு உறுதி செய்யப்பட்டது',
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு {{.DateTime}} அன்று உறுதி செய்யப்பட்டுள்ளது.'),

('booking_confirmed', 'in_app', 'en', 'normal', 'Appointment confirmed',
 'Your appointment with Dr. {{.DoctorName}} is confirmed for {{.DateTime}}.'),
('booking_confirmed', 'in_app', 'si', 'normal', 'හමුව තහවුරු කරන ලදී',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව {{.DateTime}} සඳහා තහවුරු කර ඇත.'),
('booking_confirmed', 'in_app', 'ta', 'normal', 'சந்திப்பு உறுதி செய்யப்பட்டது',
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு {{.DateTime}} அன்று உறுதி செய்யப்பட்டுள்ளது.'),

('booking_confirmed', 'email', 'en', 'normal', 'Your appointment with Dr. {{.DoctorName}} is confirmed',
 '<p>Your appointment with Dr. {{.DoctorName}} is confirmed for {{.DateTime}}.</p><p>Fee: {{.FeeLKR}}</p><p>You will receive the link to join shortly before your appointment time.</p>'),
('booking_confirmed', 'email', 'si', 'normal', 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව තහවුරු කර ඇත',
 '<p>ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව {{.DateTime}} සඳහා තහවුරු කර ඇත.</p><p>ගාස්තුව: {{.FeeLKR}}</p><p>හමුවීමේ වේලාවට පෙර සම්බන්ධ වීමේ විස්තර ඔබට එවනු ලැබේ.</p>'),
('booking_confirmed', 'email', 'ta', 'normal', 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு உறுதி செய்யப்பட்டது',
 '<p>மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு {{.DateTime}} அன்று உறுதி செய்யப்பட்டுள்ளது.</p><p>கட்டணம்: {{.FeeLKR}}</p><p>சந்திப்பு நேரத்திற்கு முன் இணையும் விவரங்கள் உங்களுக்கு அனுப்பப்படும்.</p>');

-- =============================================================================
-- reminder_1h (urgent -- overrides quiet hours) -- sms, push, in_app
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('reminder_1h', 'sms', 'en', 'urgent', NULL,
 'Reminder: your appointment with Dr. {{.DoctorName}} starts in 1 hour. Please be ready to join.'),
('reminder_1h', 'sms', 'si', 'urgent', NULL,
 'මතක් කිරීමයි: ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව පැයකින් ආරම්භ වේ. කරුණාකර සූදානම් වන්න.'),
('reminder_1h', 'sms', 'ta', 'urgent', NULL,
 'நினைவூட்டல்: மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு ஒரு மணி நேரத்தில் தொடங்குகிறது. தயவுசெய்து தயாராக இருங்கள்.'),

('reminder_1h', 'push', 'en', 'urgent', 'Appointment in 1 hour',
 'Your appointment with Dr. {{.DoctorName}} starts in 1 hour.'),
('reminder_1h', 'push', 'si', 'urgent', 'පැයකින් හමුවක්',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව පැයකින් ආරම්භ වේ.'),
('reminder_1h', 'push', 'ta', 'urgent', 'ஒரு மணி நேரத்தில் சந்திப்பு',
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு ஒரு மணி நேரத்தில் தொடங்குகிறது.'),

('reminder_1h', 'in_app', 'en', 'urgent', 'Appointment in 1 hour',
 'Your appointment with Dr. {{.DoctorName}} starts in 1 hour.'),
('reminder_1h', 'in_app', 'si', 'urgent', 'පැයකින් හමුවක්',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව පැයකින් ආරම්භ වේ.'),
('reminder_1h', 'in_app', 'ta', 'urgent', 'ஒரு மணி நேரத்தில் சந்திப்பு',
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு ஒரு மணி நேரத்தில் தொடங்குகிறது.');

-- =============================================================================
-- reminder_24h (normal) -- push, in_app, email
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('reminder_24h', 'push', 'en', 'normal', 'Appointment tomorrow',
 'Your appointment with Dr. {{.DoctorName}} is on {{.DateTime}}.'),
('reminder_24h', 'push', 'si', 'normal', 'හෙට හමුවක් ඇත',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව {{.DateTime}} දින නියමිතය.'),
('reminder_24h', 'push', 'ta', 'normal', 'நாளை சந்திப்பு உள்ளது',
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு {{.DateTime}} அன்று நிர்ணயிக்கப்பட்டுள்ளது.'),

('reminder_24h', 'in_app', 'en', 'normal', 'Appointment tomorrow',
 'Your appointment with Dr. {{.DoctorName}} is on {{.DateTime}}.'),
('reminder_24h', 'in_app', 'si', 'normal', 'හෙට හමුවක් ඇත',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව {{.DateTime}} දින නියමිතය.'),
('reminder_24h', 'in_app', 'ta', 'normal', 'நாளை சந்திப்பு உள்ளது',
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு {{.DateTime}} அன்று நிர்ணயிக்கப்பட்டுள்ளது.'),

('reminder_24h', 'email', 'en', 'normal', 'Reminder: your appointment tomorrow with Dr. {{.DoctorName}}',
 '<p>This is a reminder. Your appointment with Dr. {{.DoctorName}} is scheduled for {{.DateTime}}.</p>'),
('reminder_24h', 'email', 'si', 'normal', 'මතක් කිරීම: හෙට ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව',
 '<p>මෙය ඔබට මතක් කිරීමකි. ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව {{.DateTime}} දින නියමිතව ඇත.</p>'),
('reminder_24h', 'email', 'ta', 'normal', 'நினைவூட்டல்: நாளை மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு',
 '<p>இது ஒரு நினைவூட்டல். மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு {{.DateTime}} அன்று நிர்ணயிக்கப்பட்டுள்ளது.</p>');

-- =============================================================================
-- doctor_approved (normal) -- email, push, in_app
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('doctor_approved', 'email', 'en', 'normal', 'Your SLMC verification has been approved',
 '<p>Dear Dr. {{.DoctorName}},</p><p>Your registration verification has been approved. You can now start accepting patient appointments.</p>'),
('doctor_approved', 'email', 'si', 'normal', 'ඔබගේ SLMC සත්‍යාපනය අනුමත කර ඇත',
 '<p>ගරු වෛද්‍ය {{.DoctorName}},</p><p>ඔබගේ ලියාපදිංචි කිරීමේ සත්‍යාපනය අනුමත කර ඇත. දැන් ඔබට රෝගී හමුවීම් භාර ගැනීම ආරම්භ කළ හැක.</p>'),
('doctor_approved', 'email', 'ta', 'normal', 'உங்கள் SLMC சரிபார்ப்பு அங்கீகரிக்கப்பட்டது',
 '<p>மதிப்பிற்குரிய மருத்துவர் {{.DoctorName}} அவர்களே,</p><p>உங்கள் பதிவு சரிபார்ப்பு அங்கீகரிக்கப்பட்டுள்ளது. இப்போது நீங்கள் நோயாளர் சந்திப்புகளை ஏற்கத் தொடங்கலாம்.</p>'),

('doctor_approved', 'push', 'en', 'normal', 'Verification approved',
 'Your doctor verification has been approved. You can now accept appointments.'),
('doctor_approved', 'push', 'si', 'normal', 'සත්‍යාපනය අනුමතයි',
 'ඔබගේ වෛද්‍ය සත්‍යාපනය අනුමත කර ඇත. දැන් හමුවීම් භාර ගත හැක.'),
('doctor_approved', 'push', 'ta', 'normal', 'சரிபார்ப்பு அங்கீகரிக்கப்பட்டது',
 'உங்கள் மருத்துவர் சரிபார்ப்பு அங்கீகரிக்கப்பட்டது. இப்போது சந்திப்புகளை ஏற்கலாம்.'),

('doctor_approved', 'in_app', 'en', 'normal', 'Verification approved',
 'Your doctor verification has been approved. You can now accept appointments.'),
('doctor_approved', 'in_app', 'si', 'normal', 'සත්‍යාපනය අනුමතයි',
 'ඔබගේ වෛද්‍ය සත්‍යාපනය අනුමත කර ඇත. දැන් හමුවීම් භාර ගත හැක.'),
('doctor_approved', 'in_app', 'ta', 'normal', 'சரிபார்ப்பு அங்கீகரிக்கப்பட்டது',
 'உங்கள் மருத்துவர் சரிபார்ப்பு அங்கீகரிக்கப்பட்டது. இப்போது சந்திப்புகளை ஏற்கலாம்.');

-- =============================================================================
-- doctor_rejected (normal) -- email, push, in_app
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('doctor_rejected', 'email', 'en', 'normal', 'Your verification application needs attention',
 '<p>Dear Dr. {{.DoctorName}},</p><p>Your verification application was not approved this time. Reason: {{.Reason}}</p><p>Please contact us for further details.</p>'),
('doctor_rejected', 'email', 'si', 'normal', 'ඔබගේ සත්‍යාපන අයදුම්පත සම්බන්ධයෙන් අවධානය අවශ්‍යයි',
 '<p>ගරු වෛද්‍ය {{.DoctorName}},</p><p>ඔබගේ සත්‍යාපන අයදුම්පත මෙවර අනුමත කළ නොහැකි විය. හේතුව: {{.Reason}}</p><p>වැඩිදුර විස්තර සඳහා අප හා සම්බන්ධ වන්න.</p>'),
('doctor_rejected', 'email', 'ta', 'normal', 'உங்கள் சரிபார்ப்பு விண்ணப்பத்திற்கு கவனம் தேவை',
 '<p>மதிப்பிற்குரிய மருத்துவர் {{.DoctorName}} அவர்களே,</p><p>உங்கள் சரிபார்ப்பு விண்ணப்பம் இந்த முறை அங்கீகரிக்கப்படவில்லை. காரணம்: {{.Reason}}</p><p>மேலும் விவரங்களுக்கு எங்களைத் தொடர்பு கொள்ளவும்.</p>'),

('doctor_rejected', 'push', 'en', 'normal', 'Verification not approved',
 'Your doctor verification was not approved. Reason: {{.Reason}}'),
('doctor_rejected', 'push', 'si', 'normal', 'සත්‍යාපනය අනුමත නොවීය',
 'ඔබගේ වෛද්‍ය සත්‍යාපනය අනුමත නොවීය. හේතුව: {{.Reason}}'),
('doctor_rejected', 'push', 'ta', 'normal', 'சரிபார்ப்பு அங்கீகரிக்கப்படவில்லை',
 'உங்கள் மருத்துவர் சரிபார்ப்பு அங்கீகரிக்கப்படவில்லை. காரணம்: {{.Reason}}'),

('doctor_rejected', 'in_app', 'en', 'normal', 'Verification not approved',
 'Your doctor verification was not approved. Reason: {{.Reason}}'),
('doctor_rejected', 'in_app', 'si', 'normal', 'සත්‍යාපනය අනුමත නොවීය',
 'ඔබගේ වෛද්‍ය සත්‍යාපනය අනුමත නොවීය. හේතුව: {{.Reason}}'),
('doctor_rejected', 'in_app', 'ta', 'normal', 'சரிபார்ப்பு அங்கீகரிக்கப்படவில்லை',
 'உங்கள் மருத்துவர் சரிபார்ப்பு அங்கீகரிக்கப்படவில்லை. காரணம்: {{.Reason}}');

-- =============================================================================
-- prescription_ready (normal) -- sms, push, email, in_app
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('prescription_ready', 'sms', 'en', 'normal', NULL,
 'Your prescription from Dr. {{.DoctorName}} is ready. Download: {{.DownloadURL}}'),
('prescription_ready', 'sms', 'si', 'normal', NULL,
 'ඔබගේ වෛද්‍ය {{.DoctorName}} විසින් නිකුත් කළ බෙහෙත් වට්ටෝරුව සූදානම්. බාගත කරන්න: {{.DownloadURL}}'),
('prescription_ready', 'sms', 'ta', 'normal', NULL,
 'மருத்துவர் {{.DoctorName}} வழங்கிய உங்கள் மருந்துச் சீட்டு தயார். பதிவிறக்கம்: {{.DownloadURL}}'),

('prescription_ready', 'push', 'en', 'normal', 'Prescription ready',
 'Your prescription from Dr. {{.DoctorName}} is ready to download.'),
('prescription_ready', 'push', 'si', 'normal', 'බෙහෙත් වට්ටෝරුව සූදානම්',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} විසින් නිකුත් කළ බෙහෙත් වට්ටෝරුව බාගත කිරීමට සූදානම්.'),
('prescription_ready', 'push', 'ta', 'normal', 'மருந்துச் சீட்டு தயார்',
 'மருத்துவர் {{.DoctorName}} வழங்கிய உங்கள் மருந்துச் சீட்டு பதிவிறக்கத்திற்குத் தயார்.'),

('prescription_ready', 'in_app', 'en', 'normal', 'Prescription ready',
 'Your prescription from Dr. {{.DoctorName}} is ready to download.'),
('prescription_ready', 'in_app', 'si', 'normal', 'බෙහෙත් වට්ටෝරුව සූදානම්',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} විසින් නිකුත් කළ බෙහෙත් වට්ටෝරුව බාගත කිරීමට සූදානම්.'),
('prescription_ready', 'in_app', 'ta', 'normal', 'மருந்துச் சீட்டு தயார்',
 'மருத்துவர் {{.DoctorName}} வழங்கிய உங்கள் மருந்துச் சீட்டு பதிவிறக்கத்திற்குத் தயார்.'),

('prescription_ready', 'email', 'en', 'normal', 'Your prescription is ready',
 '<p>Your prescription from Dr. {{.DoctorName}} is ready.</p><p><a href="{{.DownloadURL}}">Download prescription</a></p>'),
('prescription_ready', 'email', 'si', 'normal', 'ඔබගේ බෙහෙත් වට්ටෝරුව සූදානම්',
 '<p>ඔබගේ වෛද්‍ය {{.DoctorName}} විසින් නිකුත් කළ බෙහෙත් වට්ටෝරුව සූදානම්.</p><p><a href="{{.DownloadURL}}">බෙහෙත් වට්ටෝරුව බාගත කරන්න</a></p>'),
('prescription_ready', 'email', 'ta', 'normal', 'உங்கள் மருந்துச் சீட்டு தயார்',
 '<p>மருத்துவர் {{.DoctorName}} வழங்கிய உங்கள் மருந்துச் சீட்டு தயார்.</p><p><a href="{{.DownloadURL}}">மருந்துச் சீட்டைப் பதிவிறக்கவும்</a></p>');

-- =============================================================================
-- payment_receipt (normal) -- email, in_app
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('payment_receipt', 'email', 'en', 'normal', 'Payment receipt - {{.AmountLKR}}',
 '<p>Your payment of {{.AmountLKR}} for your appointment with Dr. {{.DoctorName}} was successful.</p><p><a href="{{.ReceiptURL}}">View receipt</a></p>'),
('payment_receipt', 'email', 'si', 'normal', 'ගෙවීම් රිසිට්පත - {{.AmountLKR}}',
 '<p>ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව සඳහා {{.AmountLKR}} ගෙවීම සාර්ථක විය.</p><p><a href="{{.ReceiptURL}}">රිසිට්පත බලන්න</a></p>'),
('payment_receipt', 'email', 'ta', 'normal', 'கட்டணப் பற்சீட்டு - {{.AmountLKR}}',
 '<p>மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்புக்கான {{.AmountLKR}} கட்டணம் வெற்றிகரமாகச் செலுத்தப்பட்டது.</p><p><a href="{{.ReceiptURL}}">பற்சீட்டைப் பார்க்கவும்</a></p>'),

('payment_receipt', 'in_app', 'en', 'normal', 'Payment received',
 'Your payment of {{.AmountLKR}} for your appointment with Dr. {{.DoctorName}} was successful.'),
('payment_receipt', 'in_app', 'si', 'normal', 'ගෙවීම ලැබුණි',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව සඳහා {{.AmountLKR}} ගෙවීම සාර්ථක විය.'),
('payment_receipt', 'in_app', 'ta', 'normal', 'கட்டணம் பெறப்பட்டது',
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்புக்கான {{.AmountLKR}} கட்டணம் வெற்றிகரமாகச் செலுத்தப்பட்டது.');

-- =============================================================================
-- payment_failed (urgent) -- sms, push, in_app
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('payment_failed', 'sms', 'en', 'urgent', NULL,
 'Your payment of {{.AmountLKR}} could not be processed. Please try again to confirm your appointment.'),
('payment_failed', 'sms', 'si', 'urgent', NULL,
 'ඔබගේ {{.AmountLKR}} ගෙවීම සිදු කළ නොහැකි විය. හමුව තහවුරු කිරීමට කරුණාකර නැවත උත්සාහ කරන්න.'),
('payment_failed', 'sms', 'ta', 'urgent', NULL,
 'உங்கள் {{.AmountLKR}} கட்டணத்தைச் செலுத்த முடியவில்லை. சந்திப்பை உறுதி செய்ய மீண்டும் முயற்சிக்கவும்.'),

('payment_failed', 'push', 'en', 'urgent', 'Payment failed',
 'Your payment of {{.AmountLKR}} could not be processed.'),
('payment_failed', 'push', 'si', 'urgent', 'ගෙවීම අසාර්ථකයි',
 'ඔබගේ {{.AmountLKR}} ගෙවීම සිදු කළ නොහැකි විය.'),
('payment_failed', 'push', 'ta', 'urgent', 'கட்டணம் தோல்வியடைந்தது',
 'உங்கள் {{.AmountLKR}} கட்டணத்தைச் செலுத்த முடியவில்லை.'),

('payment_failed', 'in_app', 'en', 'urgent', 'Payment failed',
 'Your payment of {{.AmountLKR}} could not be processed.'),
('payment_failed', 'in_app', 'si', 'urgent', 'ගෙවීම අසාර්ථකයි',
 'ඔබගේ {{.AmountLKR}} ගෙවීම සිදු කළ නොහැකි විය.'),
('payment_failed', 'in_app', 'ta', 'urgent', 'கட்டணம் தோல்வியடைந்தது',
 'உங்கள் {{.AmountLKR}} கட்டணத்தைச் செலுத்த முடியவில்லை.');

-- =============================================================================
-- payout_sent (normal) -- email, in_app
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('payout_sent', 'email', 'en', 'normal', 'Payout sent - {{.AmountLKR}}',
 '<p>Dear Dr. {{.DoctorName}},</p><p>A payout of {{.AmountLKR}} has been sent to your registered bank account.</p>'),
('payout_sent', 'email', 'si', 'normal', 'ගෙවීම එවන ලදී - {{.AmountLKR}}',
 '<p>ගරු වෛද්‍ය {{.DoctorName}},</p><p>{{.AmountLKR}} ගෙවීමක් ඔබගේ ලියාපදිංචි බැංකු ගිණුමට එවා ඇත.</p>'),
('payout_sent', 'email', 'ta', 'normal', 'பணம் அனுப்பப்பட்டது - {{.AmountLKR}}',
 '<p>மதிப்பிற்குரிய மருத்துவர் {{.DoctorName}} அவர்களே,</p><p>{{.AmountLKR}} தொகை உங்கள் பதிவுசெய்யப்பட்ட வங்கிக் கணக்கிற்கு அனுப்பப்பட்டுள்ளது.</p>'),

('payout_sent', 'in_app', 'en', 'normal', 'Payout sent',
 'A payout of {{.AmountLKR}} has been sent to your account.'),
('payout_sent', 'in_app', 'si', 'normal', 'ගෙවීම එවන ලදී',
 '{{.AmountLKR}} ගෙවීමක් ඔබගේ ගිණුමට එවා ඇත.'),
('payout_sent', 'in_app', 'ta', 'normal', 'பணம் அனுப்பப்பட்டது',
 '{{.AmountLKR}} தொகை உங்கள் கணக்கிற்கு அனுப்பப்பட்டுள்ளது.');

-- =============================================================================
-- waitlist_available (urgent -- 5 minute reservation window) -- sms, push, in_app
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('waitlist_available', 'sms', 'en', 'urgent', NULL,
 'A slot with Dr. {{.DoctorName}} is now available. Book within {{.ExpiresInMinutes}} minutes to secure it.'),
('waitlist_available', 'sms', 'si', 'urgent', NULL,
 'වෛද්‍ය {{.DoctorName}} සමඟ හිස් වේලාවක් දැන් ලබා ගත හැක. මිනිත්තු {{.ExpiresInMinutes}} ක් ඇතුළත වෙන් කරගන්න.'),
('waitlist_available', 'sms', 'ta', 'urgent', NULL,
 'மருத்துவர் {{.DoctorName}} உடன் ஒரு காலியிடம் இப்போது கிடைக்கிறது. {{.ExpiresInMinutes}} நிமிடங்களுக்குள் முன்பதிவு செய்யவும்.'),

('waitlist_available', 'push', 'en', 'urgent', 'Slot available',
 'A slot with Dr. {{.DoctorName}} is available for the next {{.ExpiresInMinutes}} minutes.'),
('waitlist_available', 'push', 'si', 'urgent', 'වේලාවක් ලැබී ඇත',
 'වෛද්‍ය {{.DoctorName}} සමඟ වේලාවක් මිනිත්තු {{.ExpiresInMinutes}} ක් සඳහා ලබා ගත හැක.'),
('waitlist_available', 'push', 'ta', 'urgent', 'காலியிடம் கிடைத்தது',
 'மருத்துவர் {{.DoctorName}} உடன் ஒரு காலியிடம் அடுத்த {{.ExpiresInMinutes}} நிமிடங்களுக்குக் கிடைக்கிறது.'),

('waitlist_available', 'in_app', 'en', 'urgent', 'Slot available',
 'A slot with Dr. {{.DoctorName}} is available for the next {{.ExpiresInMinutes}} minutes.'),
('waitlist_available', 'in_app', 'si', 'urgent', 'වේලාවක් ලැබී ඇත',
 'වෛද්‍ය {{.DoctorName}} සමඟ වේලාවක් මිනිත්තු {{.ExpiresInMinutes}} ක් සඳහා ලබා ගත හැක.'),
('waitlist_available', 'in_app', 'ta', 'urgent', 'காலியிடம் கிடைத்தது',
 'மருத்துவர் {{.DoctorName}} உடன் ஒரு காலியிடம் அடுத்த {{.ExpiresInMinutes}} நிமிடங்களுக்குக் கிடைக்கிறது.');

-- =============================================================================
-- consultation_starting (urgent) -- push, sms, in_app
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('consultation_starting', 'sms', 'en', 'urgent', NULL,
 'Your consultation with Dr. {{.DoctorName}} is starting now. Join: {{.JoinLink}}'),
('consultation_starting', 'sms', 'si', 'urgent', NULL,
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව දැන් ආරම්භ වේ. සම්බන්ධ වන්න: {{.JoinLink}}'),
('consultation_starting', 'sms', 'ta', 'urgent', NULL,
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் ஆலோசனை இப்போது தொடங்குகிறது. இணையவும்: {{.JoinLink}}'),

('consultation_starting', 'push', 'en', 'urgent', 'Consultation starting',
 'Your consultation with Dr. {{.DoctorName}} is starting now.'),
('consultation_starting', 'push', 'si', 'urgent', 'හමුව ආරම්භ වේ',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව දැන් ආරම්භ වේ.'),
('consultation_starting', 'push', 'ta', 'urgent', 'ஆலோசனை தொடங்குகிறது',
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் ஆலோசனை இப்போது தொடங்குகிறது.'),

('consultation_starting', 'in_app', 'en', 'urgent', 'Consultation starting',
 'Your consultation with Dr. {{.DoctorName}} is starting now.'),
('consultation_starting', 'in_app', 'si', 'urgent', 'හමුව ආරම්භ වේ',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව දැන් ආරම්භ වේ.'),
('consultation_starting', 'in_app', 'ta', 'urgent', 'ஆலோசனை தொடங்குகிறது',
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் ஆலோசனை இப்போது தொடங்குகிறது.');

-- =============================================================================
-- appointment_cancelled (normal) -- sms, push, email, in_app
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('appointment_cancelled', 'sms', 'en', 'normal', NULL,
 'Your appointment with Dr. {{.DoctorName}} on {{.DateTime}} has been cancelled. {{.Reason}}'),
('appointment_cancelled', 'sms', 'si', 'normal', NULL,
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව ({{.DateTime}}) අවලංගු කර ඇත. {{.Reason}}'),
('appointment_cancelled', 'sms', 'ta', 'normal', NULL,
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு ({{.DateTime}}) ரத்து செய்யப்பட்டது. {{.Reason}}'),

('appointment_cancelled', 'push', 'en', 'normal', 'Appointment cancelled',
 'Your appointment with Dr. {{.DoctorName}} on {{.DateTime}} has been cancelled.'),
('appointment_cancelled', 'push', 'si', 'normal', 'හමුව අවලංගු කරන ලදී',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව ({{.DateTime}}) අවලංගු කර ඇත.'),
('appointment_cancelled', 'push', 'ta', 'normal', 'சந்திப்பு ரத்து செய்யப்பட்டது',
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு ({{.DateTime}}) ரத்து செய்யப்பட்டது.'),

('appointment_cancelled', 'in_app', 'en', 'normal', 'Appointment cancelled',
 'Your appointment with Dr. {{.DoctorName}} on {{.DateTime}} has been cancelled.'),
('appointment_cancelled', 'in_app', 'si', 'normal', 'හමුව අවලංගු කරන ලදී',
 'ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව ({{.DateTime}}) අවලංගු කර ඇත.'),
('appointment_cancelled', 'in_app', 'ta', 'normal', 'சந்திப்பு ரத்து செய்யப்பட்டது',
 'மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு ({{.DateTime}}) ரத்து செய்யப்பட்டது.'),

('appointment_cancelled', 'email', 'en', 'normal', 'Your appointment has been cancelled',
 '<p>Your appointment with Dr. {{.DoctorName}} on {{.DateTime}} has been cancelled.</p><p>Reason: {{.Reason}}</p>'),
('appointment_cancelled', 'email', 'si', 'normal', 'ඔබගේ හමුව අවලංගු කර ඇත',
 '<p>ඔබගේ වෛද්‍ය {{.DoctorName}} හමුව {{.DateTime}} සඳහා අවලංගු කර ඇත.</p><p>හේතුව: {{.Reason}}</p>'),
('appointment_cancelled', 'email', 'ta', 'normal', 'உங்கள் சந்திப்பு ரத்து செய்யப்பட்டது',
 '<p>மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு {{.DateTime}} அன்று ரத்து செய்யப்பட்டது.</p><p>காரணம்: {{.Reason}}</p>');

-- =============================================================================
-- otp_code (critical -- bypasses preferences and quiet hours) -- sms only
-- =============================================================================
INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('otp_code', 'sms', 'en', 'critical', NULL,
 'Your verification code is {{.Code}}. It expires in {{.ExpiresInMinutes}} minutes. Do not share this code with anyone.'),
('otp_code', 'sms', 'si', 'critical', NULL,
 'ඔබගේ තහවුරු කිරීමේ කේතය {{.Code}} වේ. එය මිනිත්තු {{.ExpiresInMinutes}} කින් කල් ඉකුත් වේ. මෙම කේතය කිසිවෙකු සමඟ බෙදා නොගන්න.'),
('otp_code', 'sms', 'ta', 'critical', NULL,
 'உங்கள் சரிபார்ப்புக் குறியீடு {{.Code}}. இது {{.ExpiresInMinutes}} நிமிடங்களில் காலாவதியாகும். இந்தக் குறியீட்டை யாருடனும் பகிர வேண்டாம்.');
