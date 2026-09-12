-- Courtesy ping when the doctor is free a few minutes early. No clinical
-- detail; doctor display name and join link only. The patient can keep the
-- original booked time.

INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('early_join_offered', 'sms', 'en', 'normal', NULL,
 'Dr. {{.DoctorName}} is free a few minutes early. Can you join now? {{.JoinLink}} — or keep your booked time.'),
('early_join_offered', 'sms', 'si', 'normal', NULL,
 'වෛද්‍ය {{.DoctorName}} මිනිත්තු කිහිපයකින් කලින් නිදහස්. දැන් එකතු විය හැකිද? {{.JoinLink}} — නැතිනම් වෙන්කළ වේලාව තබා ගන්න.'),
('early_join_offered', 'sms', 'ta', 'normal', NULL,
 'மருத்துவர் {{.DoctorName}} சில நிமிடங்கள் முன்னதாக காலியாக இருக்கிறார். இப்போது இணைய முடியுமா? {{.JoinLink}} — அல்லது முன்பதிவு நேரத்தை வைத்துக்கொள்ளுங்கள்.'),

('early_join_offered', 'push', 'en', 'normal', 'Doctor is free a few minutes early',
 'Dr. {{.DoctorName}} can see you now if you are ready. You can also keep your booked time.'),
('early_join_offered', 'push', 'si', 'normal', 'වෛද්‍යවරයා මිනිත්තු කිහිපයකින් කලින් නිදහස්',
 'වෛද්‍ය {{.DoctorName}} දැන් ඔබව දැකිය හැකිය. ඔබට වෙන්කළ වේලාව තබා ගත හැකිය.'),
('early_join_offered', 'push', 'ta', 'normal', 'மருத்துவர் சில நிமிடங்கள் முன்னதாக காலியாக இருக்கிறார்',
 'மருத்துவர் {{.DoctorName}} இப்போது உங்களைப் பார்க்க முடியும். உங்கள் முன்பதிவு நேரத்தையும் வைத்துக்கொள்ளலாம்.'),

('early_join_offered', 'email', 'en', 'normal',
 'Dr. {{.DoctorName}} is free a few minutes early',
 '<p>Dr. {{.DoctorName}} is free a few minutes early and can see you now if you are ready.</p><p>Join now: {{.JoinLink}}</p><p>If now is not convenient, keep your original booked time — nothing else changes.</p>'),
('early_join_offered', 'email', 'si', 'normal',
 'වෛද්‍ය {{.DoctorName}} මිනිත්තු කිහිපයකින් කලින් නිදහස්',
 '<p>වෛද්‍ය {{.DoctorName}} මිනිත්තු කිහිපයකින් කලින් නිදහස්. ඔබ සූදානම් නම් දැන් එකතු විය හැකිය.</p><p>දැන් සම්බන්ධ වන්න: {{.JoinLink}}</p><p>දැන් අපහසු නම්, වෙන්කළ වේලාව තබා ගන්න — වෙනත් කිසිවක් වෙනස් නොවේ.</p>'),
('early_join_offered', 'email', 'ta', 'normal',
 'மருத்துவர் {{.DoctorName}} சில நிமிடங்கள் முன்னதாக காலியாக இருக்கிறார்',
 '<p>மருத்துவர் {{.DoctorName}} சில நிமிடங்கள் முன்னதாக காலியாக இருக்கிறார். நீங்கள் தயாராக இருந்தால் இப்போது இணையலாம்.</p><p>இப்போது இணையவும்: {{.JoinLink}}</p><p>இப்போது வசதியாக இல்லையென்றால், உங்கள் முன்பதிவு நேரத்தை வைத்துக்கொள்ளுங்கள் — வேறு எதுவும் மாறாது.</p>'),

('early_join_offered', 'in_app', 'en', 'normal', 'Doctor is free a few minutes early',
 'Dr. {{.DoctorName}} can see you now if you are ready. You can also keep your booked time.'),
('early_join_offered', 'in_app', 'si', 'normal', 'වෛද්‍යවරයා මිනිත්තු කිහිපයකින් කලින් නිදහස්',
 'වෛද්‍ය {{.DoctorName}} දැන් ඔබව දැකිය හැකිය. ඔබට වෙන්කළ වේලාව තබා ගත හැකිය.'),
('early_join_offered', 'in_app', 'ta', 'normal', 'மருத்துவர் சில நிமிடங்கள் முன்னதாக காலியாக இருக்கிறார்',
 'மருத்துவர் {{.DoctorName}} இப்போது உங்களைப் பார்க்க முடியும். உங்கள் முன்பதிவு நேரத்தையும் வைத்துக்கொள்ளலாம்.');
