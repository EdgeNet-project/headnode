/*
Copyright 2020 Sorbonne Université

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package userregistrationrequest

import (
	"fmt"
	"math/rand"
	"time"

	apps_v1alpha "headnode/pkg/apis/apps/v1alpha"
	"headnode/pkg/authorization"
	"headnode/pkg/client/clientset/versioned"
	"headnode/pkg/mailer"
	"headnode/pkg/registration"

	log "github.com/Sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// HandlerInterface interface contains the methods that are required
type HandlerInterface interface {
	Init() error
	ObjectCreated(obj interface{})
	ObjectUpdated(obj interface{})
	ObjectDeleted(obj interface{})
}

// Handler implementation
type Handler struct {
	clientset        *kubernetes.Clientset
	edgenetClientset *versioned.Clientset
}

// Init handles any handler initialization
func (t *Handler) Init() error {
	log.Info("URRHandler.Init")
	var err error
	t.clientset, err = authorization.CreateClientSet()
	if err != nil {
		log.Println(err.Error())
		panic(err.Error())
	}
	t.edgenetClientset, err = authorization.CreateEdgeNetClientSet()
	if err != nil {
		log.Println(err.Error())
		panic(err.Error())
	}
	return err
}

// ObjectCreated is called when an object is created
func (t *Handler) ObjectCreated(obj interface{}) {
	log.Info("URRHandler.ObjectCreated")
	// Create a copy of the user registration request object to make changes on it
	URRCopy := obj.(*apps_v1alpha.UserRegistrationRequest).DeepCopy()
	// Check if the email address is already taken
	exist := t.checkUsernameEmailAddress(URRCopy)
	if exist {
		// If it is already taken, remove the user registration request object
		t.edgenetClientset.AppsV1alpha().UserRegistrationRequests(URRCopy.GetNamespace()).Delete(URRCopy.GetName(), &metav1.DeleteOptions{})
		return
	}
	// Find the site from the namespace in which the object is
	URROwnerNamespace, _ := t.clientset.CoreV1().Namespaces().Get(URRCopy.GetNamespace(), metav1.GetOptions{})
	URROwnerSite, _ := t.edgenetClientset.AppsV1alpha().Sites().Get(URROwnerNamespace.Labels["site-name"], metav1.GetOptions{})
	// Check if the site is active
	if URROwnerSite.Status.Enabled {
		// If the service restarts, it creates all objects again
		// Because of that, this section covers a variety of possibilities
		if URRCopy.Status.Expires == nil {
			// Create a user-specific secret to keep the password safe
			passwordSecret := registration.CreateSecretByPassword(URRCopy)
			// Update the password field as the secret's name for later use
			URRCopy.Spec.Password = passwordSecret
			URRCopyUpdated, _ := t.edgenetClientset.AppsV1alpha().UserRegistrationRequests(URRCopy.GetNamespace()).Update(URRCopy)
			// Run timeout goroutine
			go t.runApprovalTimeout(URRCopyUpdated)
			defer t.edgenetClientset.AppsV1alpha().UserRegistrationRequests(URRCopyUpdated.GetNamespace()).UpdateStatus(URRCopyUpdated)
			URRCopyUpdated.Status.Approved = false
			// Set the approval timeout which is 72 hours
			URRCopyUpdated.Status.Expires = &metav1.Time{
				Time: time.Now().Add(72 * time.Hour),
			}
			// The section below is a part of the method which provides email verification
			// Email verification code is a security point for email verification. The user
			// registration object creates an email verification object with a name which is
			// this email verification code. Only who knows the site and the email verification
			// code can manipulate that object by using a public token.
			URROwnerReferences := t.setOwnerReferences(URRCopyUpdated)
			emailVerificationCode := "bs" + generateRandomString(16)
			emailVerification := apps_v1alpha.EmailVerification{ObjectMeta: metav1.ObjectMeta{OwnerReferences: URROwnerReferences}}
			emailVerification.SetName(emailVerificationCode)
			emailVerification.Spec.Kind = "User"
			emailVerification.Spec.Identifier = URRCopy.GetName()
			_, err := t.edgenetClientset.AppsV1alpha().EmailVerifications(URRCopy.GetNamespace()).Create(emailVerification.DeepCopy())
			if err == nil {
				// Set the HTML template variables
				contentData := mailer.VerifyContentData{}
				contentData.CommonData.Site = URROwnerNamespace.Labels["site-name"]
				contentData.CommonData.Username = URRCopyUpdated.GetName()
				contentData.CommonData.Name = fmt.Sprintf("%s %s", URRCopyUpdated.Spec.FirstName, URRCopyUpdated.Spec.LastName)
				contentData.CommonData.Email = []string{URRCopyUpdated.Spec.Email}
				contentData.Code = emailVerificationCode
				mailer.Send("user-email-verification", contentData)
			}
		} else {
			go t.runApprovalTimeout(URRCopy)
		}
	} else {
		t.edgenetClientset.AppsV1alpha().UserRegistrationRequests(URRCopy.GetNamespace()).Delete(URRCopy.GetName(), &metav1.DeleteOptions{})
	}
}

// ObjectUpdated is called when an object is updated
func (t *Handler) ObjectUpdated(obj interface{}) {
	log.Info("URRHandler.ObjectUpdated")
	// Create a copy of the user registration request object to make changes on it
	URRCopy := obj.(*apps_v1alpha.UserRegistrationRequest).DeepCopy()
	URROwnerNamespace, _ := t.clientset.CoreV1().Namespaces().Get(URRCopy.GetNamespace(), metav1.GetOptions{})
	URROwnerSite, _ := t.edgenetClientset.AppsV1alpha().Sites().Get(URROwnerNamespace.Labels["site-name"], metav1.GetOptions{})

	if URROwnerSite.Status.Enabled {
		// Check whether the request for user registration approved
		if URRCopy.Status.Approved {
			// Check again if the email address is already taken
			exist := t.checkUsernameEmailAddress(URRCopy)
			if !exist {
				// Create a user on site
				user := apps_v1alpha.User{}
				user.SetName(URRCopy.GetName())
				user.Spec.Bio = URRCopy.Spec.Bio
				user.Spec.Email = URRCopy.Spec.Email
				user.Spec.FirstName = URRCopy.Spec.FirstName
				user.Spec.LastName = URRCopy.Spec.LastName
				user.Spec.Password = URRCopy.Spec.Password
				user.Spec.Roles = URRCopy.Spec.Roles
				user.Spec.URL = URRCopy.Spec.URL
				userCreated, _ := t.edgenetClientset.AppsV1alpha().Users(URRCopy.GetNamespace()).Create(user.DeepCopy())

				// Add the user created as an owner reference to password secret since the user registration object will be removed
				passwordSecret, _ := t.clientset.CoreV1().Secrets(URRCopy.GetNamespace()).Get(fmt.Sprintf("%s-pass", URRCopy.GetName()), metav1.GetOptions{})
				newSecretRef := *metav1.NewControllerRef(userCreated, apps_v1alpha.SchemeGroupVersion.WithKind("User"))
				takeControl := false
				newSecretRef.Controller = &takeControl
				passwordSecret.OwnerReferences = append(passwordSecret.OwnerReferences, newSecretRef)
				t.clientset.CoreV1().Secrets(URRCopy.GetNamespace()).Update(passwordSecret)
			}
			t.edgenetClientset.AppsV1alpha().UserRegistrationRequests(URRCopy.GetNamespace()).Delete(URRCopy.GetName(), &metav1.DeleteOptions{})
		}
	} else {
		t.edgenetClientset.AppsV1alpha().UserRegistrationRequests(URRCopy.GetNamespace()).Delete(URRCopy.GetName(), &metav1.DeleteOptions{})
	}
}

// ObjectDeleted is called when an object is deleted
func (t *Handler) ObjectDeleted(obj interface{}) {
	log.Info("URRHandler.ObjectDeleted")
	// Mail notification, TBD
}

// runApprovalTimeout puts a procedure in place to remove requests by approval or timeout
func (t *Handler) runApprovalTimeout(URRCopy *apps_v1alpha.UserRegistrationRequest) {
	registrationApproved := make(chan bool, 1)
	timeoutRenewed := make(chan bool, 1)
	terminated := make(chan bool, 1)
	var timeout <-chan time.Time
	if URRCopy.Status.Expires != nil {
		timeout = time.After(time.Until(URRCopy.Status.Expires.Time))
	}
	closeChannels := func() {
		close(registrationApproved)
		close(timeoutRenewed)
		close(terminated)
	}

	// Watch the events of user registration request object
	watchURR, err := t.edgenetClientset.AppsV1alpha().UserRegistrationRequests(URRCopy.GetNamespace()).Watch(metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name==%s", URRCopy.GetName())})
	if err == nil {
		go func() {
			// Get events from watch interface
			for URREvent := range watchURR.ResultChan() {
				// Get updated user registration request object
				updatedURR, status := URREvent.Object.(*apps_v1alpha.UserRegistrationRequest)
				if status {
					if URREvent.Type == "DELETED" {
						terminated <- true
						continue
					}

					if updatedURR.Status.Approved == true {
						registrationApproved <- true
						break
					} else if updatedURR.Status.Expires != nil {
						timeout = time.After(time.Until(updatedURR.Status.Expires.Time))
						// Check whether expiration date updated
						if URRCopy.Status.Expires != nil {
							if URRCopy.Status.Expires.Time != updatedURR.Status.Expires.Time {
								timeoutRenewed <- true
							}
						} else {
							timeoutRenewed <- true
						}
					}
				}
			}
		}()
	} else {
		// In case of any malfunction of watching userregistrationrequest resources,
		// there is a timeout at 72 hours
		timeout = time.After(72 * time.Hour)
	}

	// Infinite loop
timeoutLoop:
	for {
		// Wait on multiple channel operations
	timeoutOptions:
		select {
		case <-registrationApproved:
			watchURR.Stop()
			closeChannels()
			break timeoutLoop
		case <-timeoutRenewed:
			break timeoutOptions
		case <-timeout:
			watchURR.Stop()
			t.edgenetClientset.AppsV1alpha().UserRegistrationRequests(URRCopy.GetNamespace()).Delete(URRCopy.GetName(), &metav1.DeleteOptions{})
			closeChannels()
			break timeoutLoop
		case <-terminated:
			watchURR.Stop()
			closeChannels()
			break timeoutLoop
		}
	}
}

// checkUsernameEmailAddress checks whether a user exists with the same username or email address
func (t *Handler) checkUsernameEmailAddress(URRCopy *apps_v1alpha.UserRegistrationRequest) bool {
	exist := false
	// To check username on the users resource
	userRaw, _ := t.edgenetClientset.AppsV1alpha().Users(URRCopy.GetNamespace()).List(
		metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name==%s", URRCopy.GetName())})
	if len(userRaw.Items) == 0 {
		// To check email address
		userRaw, _ = t.edgenetClientset.AppsV1alpha().Users("").List(metav1.ListOptions{})
		for _, userRow := range userRaw.Items {
			if userRow.Spec.Email == URRCopy.Spec.Email {
				exist = true
				break
			}
		}
	} else {
		exist = true
	}

	if !exist {
		// To check email address
		URRRaw, _ := t.edgenetClientset.AppsV1alpha().UserRegistrationRequests("").List(metav1.ListOptions{})
		for _, URRRow := range URRRaw.Items {
			if URRRow.Spec.Email == URRCopy.Spec.Email && URRRow.GetUID() != URRCopy.GetUID() {
				exist = true
			}
		}
	}
	// Mail notification, TBD
	return exist
}

// setOwnerReferences put the userregistrationrequest as owner
func (t *Handler) setOwnerReferences(URRCopy *apps_v1alpha.UserRegistrationRequest) []metav1.OwnerReference {
	ownerReferences := []metav1.OwnerReference{}
	newNamespaceRef := *metav1.NewControllerRef(URRCopy, apps_v1alpha.SchemeGroupVersion.WithKind("UserRegistrationRequest"))
	takeControl := false
	newNamespaceRef.Controller = &takeControl
	ownerReferences = append(ownerReferences, newNamespaceRef)
	return ownerReferences
}

// generateRandomString to have a unique string
func generateRandomString(n int) string {
	var letter = []rune("abcdefghijklmnopqrstuvwxyz0123456789")

	b := make([]rune, n)
	rand.Seed(time.Now().UnixNano())
	for i := range b {
		b[i] = letter[rand.Intn(len(letter))]
	}
	return string(b)
}
