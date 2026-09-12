-- Courtesy ping to the next patient while the doctor is still finishing the
-- previous visit. No clinical detail; doctor display name only.

INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('doctor_running_late', 'sms', 'en', 'normal', NULL,
 'Dr. {{.DoctorName}} is currently with the previous patient. Please stay with us — we are sorry for the short delay.'),
('doctor_running_late', 'sms', 'si', 'normal', NULL,
 'වෛද්‍ය {{.DoctorName}} දැන් පෙර රෝගීන් සමඟ ඇත. කරුණාකර අප සමඟ සිටින්න — කෙටි ප්‍රමාදයට කණගාටුයි.'),
('doctor_running_late', 'sms', 'ta', 'normal', NULL,
 'மருத்துவர் {{.DoctorName}} தற்போது முந்தைய நோயாளியுடன் உள்ளார். தயவுசெய்து எங்களுடன் இருங்கள் — சிறு தாமதத்திற்கு வருந்துகிறோம்.'),

('doctor_running_late', 'push', 'en', 'normal', 'Doctor running a little late',
 'Dr. {{.DoctorName}} is finishing with the previous patient. Please stay with us — sorry for the short wait.'),
('doctor_running_late', 'push', 'si', 'normal', 'වෛද්‍යවරයා ටිකක් ප්‍රමාදයි',
 'වෛද්‍ය {{.DoctorName}} පෙර රෝගියා සමඟ අවසන් කරමින් සිටී. කරුණාකර අප සමඟ සිටින්න — කෙටි ප්‍රමාදයට කණගාටුයි.'),
('doctor_running_late', 'push', 'ta', 'normal', 'மருத்துவர் சிறிது தாமதம்',
 'மருத்துவர் {{.DoctorName}} முந்தைய நோயாளியுடன் முடித்துக் கொண்டிருக்கிறார். தயவுசெய்து எங்களுடன் இருங்கள் — சிறு காத்திருப்புக்கு வருந்துகிறோம்.'),

('doctor_running_late', 'email', 'en', 'normal',
 'Dr. {{.DoctorName}} is running a little late',
 '<p>Dr. {{.DoctorName}} is currently with the previous patient.</p><p>Please stay with us — we are sorry for the short delay. Your visit will begin shortly.</p>'),
('doctor_running_late', 'email', 'si', 'normal',
 'වෛද්‍ය {{.DoctorName}} ටිකක් ප්‍රමාදයි',
 '<p>වෛද්‍ය {{.DoctorName}} දැන් පෙර රෝගීන් සමඟ ඇත.</p><p>කරුණාකර අප සමඟ සිටින්න — කෙටි ප්‍රමාදයට කණගාටුයි. ඔබගේ හමුව ඉක්මනින් ආරම්භ වේ.</p>'),
('doctor_running_late', 'email', 'ta', 'normal',
 'மருத்துவர் {{.DoctorName}} சிறிது தாமதம்',
 '<p>மருத்துவர் {{.DoctorName}} தற்போது முந்தைய நோயாளியுடன் உள்ளார்.</p><p>தயவுசெய்து எங்களுடன் இருங்கள் — சிறு தாமதத்திற்கு வருந்துகிறோம். உங்கள் சந்திப்பு விரைவில் தொடங்கும்.</p>'),

('doctor_running_late', 'in_app', 'en', 'normal', 'Doctor running a little late',
 'Dr. {{.DoctorName}} is currently with the previous patient. Please stay with us — sorry for the short delay.'),
('doctor_running_late', 'in_app', 'si', 'normal', 'වෛද්‍යවරයා ටිකක් ප්‍රමාදයි',
 'වෛද්‍ය {{.DoctorName}} දැන් පෙර රෝගීන් සමඟ ඇත. කරුණාකර අප සමඟ සිටින්න — කෙටි ප්‍රමාදයට කණගාටුයි.'),
('doctor_running_late', 'in_app', 'ta', 'normal', 'மருத்துவர் சிறிது தாமதம்',
 'மருத்துவர் {{.DoctorName}} தற்போது முந்தைய நோயாளியுடன் உள்ளார். தயவுசெய்து எங்களுடன் இருங்கள் — சிறு தாமதத்திற்கு வருந்துகிறோம்.');
