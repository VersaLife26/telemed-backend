UPDATE templates
SET subject_template = 'Your doctor application was approved — you can sign in',
    body_template = '<p>Dear Dr. {{.DoctorName}},</p><p>Your application has been approved. Sign in at the doctor portal with the email and password you used when you applied.</p><p>{{.PortalURL}}</p>'
WHERE key = 'doctor_application_approved' AND channel = 'email' AND locale = 'en';

UPDATE templates
SET subject_template = 'ඔබගේ වෛද්‍ය අයදුම්පත අනුමතයි — පිවිසෙන්න',
    body_template = '<p>ගරු වෛද්‍ය {{.DoctorName}},</p><p>ඔබගේ අයදුම්පත අනුමත කර ඇත. ඔබ අයදුම් කිරීමේදී භාවිත කළ ඊමේල් සහ මුරපදයෙන් වෛද්‍ය ද්වාරයට පිවිසෙන්න.</p><p>{{.PortalURL}}</p>'
WHERE key = 'doctor_application_approved' AND channel = 'email' AND locale = 'si';

UPDATE templates
SET subject_template = 'உங்கள் மருத்துவர் விண்ணப்பம் அங்கீகரிக்கப்பட்டது — உள்நுழையலாம்',
    body_template = '<p>மதிப்பிற்குரிய மருத்துவர் {{.DoctorName}} அவர்களே,</p><p>உங்கள் விண்ணப்பம் அங்கீகரிக்கப்பட்டது. நீங்கள் விண்ணப்பித்த மின்னஞ்சல் மற்றும் கடவுச்சொல்லால் மருத்துவர் போர்ட்டலில் உள்நுழையவும்.</p><p>{{.PortalURL}}</p>'
WHERE key = 'doctor_application_approved' AND channel = 'email' AND locale = 'ta';
