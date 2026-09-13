-- Two more shapes of "your application was approved".
--
-- 000011 rewrote the single approval email to say "sign in with the email and
-- password you used when you applied". That is right for most applicants and
-- wrong for two of them:
--
--   * the applicant who already had a VersaLife account on that email. Approval
--     promotes it to a doctor account but deliberately does NOT overwrite its
--     password (that would make the apply form a password-reset for anyone
--     else's address), so the password they just typed is not the one that
--     works.
--   * the applicant approved on a deployment with no account provisioner
--     configured, where no login exists yet and phone OTP is the only way in.
--
-- doctor.application_approved now carries login_ready and password_applied,
-- and the consumer picks between these three keys.

INSERT INTO templates (key, channel, locale, urgency, subject_template, body_template) VALUES
('doctor_application_approved_existing', 'email', 'en', 'normal',
 'Your doctor application was approved — you can sign in',
 '<p>Dear Dr. {{.DoctorName}},</p><p>Your application has been approved and your VersaLife account is now a doctor account.</p><p>This email address already had a VersaLife account, so <strong>keep using the password you already had</strong> — not the one you chose on the application form. If you do not remember it, sign in with your mobile number instead and set a new password from your profile.</p><p>{{.PortalURL}}</p>'),
('doctor_application_approved_existing', 'email', 'si', 'normal',
 'ඔබගේ වෛද්‍ය අයදුම්පත අනුමතයි — පිවිසෙන්න',
 '<p>ගරු වෛද්‍ය {{.DoctorName}},</p><p>ඔබගේ අයදුම්පත අනුමත කර ඇති අතර ඔබගේ ගිණුම දැන් වෛද්‍ය ගිණුමකි.</p><p>මෙම ඊමේල් ලිපිනයට දැනටමත් VersaLife ගිණුමක් තිබූ නිසා, <strong>ඔබ දැනටමත් භාවිත කළ මුරපදය</strong> භාවිතා කරන්න — අයදුම්පතේ තෝරාගත් එක නොවේ. එය මතක නැත්නම්, ජංගම දුරකථන අංකයෙන් පිවිස පැතිකඩෙන් නව මුරපදයක් සකසන්න.</p><p>{{.PortalURL}}</p>'),
('doctor_application_approved_existing', 'email', 'ta', 'normal',
 'உங்கள் மருத்துவர் விண்ணப்பம் அங்கீகரிக்கப்பட்டது — உள்நுழையலாம்',
 '<p>மதிப்பிற்குரிய மருத்துவர் {{.DoctorName}} அவர்களே,</p><p>உங்கள் விண்ணப்பம் அங்கீகரிக்கப்பட்டது; உங்கள் கணக்கு இப்போது மருத்துவர் கணக்காக மாற்றப்பட்டுள்ளது.</p><p>இந்த மின்னஞ்சலுக்கு ஏற்கனவே VersaLife கணக்கு இருந்ததால், <strong>நீங்கள் ஏற்கனவே பயன்படுத்திய கடவுச்சொல்லையே</strong> பயன்படுத்தவும் — விண்ணப்பத்தில் தேர்ந்தெடுத்ததை அல்ல. நினைவில் இல்லையெனில், கைபேசி எண்ணால் உள்நுழைந்து சுயவிவரத்தில் புதிய கடவுச்சொல் அமைக்கவும்.</p><p>{{.PortalURL}}</p>'),

('doctor_application_approved_otp', 'email', 'en', 'normal',
 'Your doctor application was approved — complete OTP',
 '<p>Dear Dr. {{.DoctorName}},</p><p>Your application has been approved. Sign in at the doctor portal with your phone number and complete OTP verification to activate your account. You can set a password from your profile afterwards.</p><p>{{.PortalURL}}</p>'),
('doctor_application_approved_otp', 'email', 'si', 'normal',
 'ඔබගේ වෛද්‍ය අයදුම්පත අනුමතයි — OTP සම්පූර්ණ කරන්න',
 '<p>ගරු වෛද්‍ය {{.DoctorName}},</p><p>ඔබගේ අයදුම්පත අනුමත කර ඇත. වෛද්‍ය ද්වාරයේ දුරකථන අංකයෙන් පිවිස OTP සම්පූර්ණ කර ගිණුම සක්‍රිය කරන්න. පසුව පැතිකඩෙන් මුරපදයක් සැකසිය හැක.</p><p>{{.PortalURL}}</p>'),
('doctor_application_approved_otp', 'email', 'ta', 'normal',
 'உங்கள் மருத்துவர் விண்ணப்பம் அங்கீகரிக்கப்பட்டது — OTP முடிக்கவும்',
 '<p>மதிப்பிற்குரிய மருத்துவர் {{.DoctorName}} அவர்களே,</p><p>உங்கள் விண்ணப்பம் அங்கீகரிக்கப்பட்டது. மருத்துவர் போர்ட்டலில் தொலைபேசி எண்ணால் உள்நுழைந்து OTP சரிபார்த்து கணக்கை செயல்படுத்தவும். பின்னர் சுயவிவரத்தில் கடவுச்சொல் அமைக்கலாம்.</p><p>{{.PortalURL}}</p>');
