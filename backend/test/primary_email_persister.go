package test

import (
	"github.com/teamhanko/hanko/backend/persistence"
	"github.com/teamhanko/hanko/backend/persistence/models"
)

func NewPrimaryEmailPersister(init []models.PrimaryEmail) persistence.PrimaryEmailPersister {
	return &primaryEmailPersister{append([]models.PrimaryEmail{}, init...)}
}

type primaryEmailPersister struct {
	primaryEmails []models.PrimaryEmail
}

func (p *primaryEmailPersister) Create(primaryEmail models.PrimaryEmail) error {
	p.primaryEmails = append(p.primaryEmails, primaryEmail)
	return nil
}

func (p *primaryEmailPersister) Update(primaryEmail models.PrimaryEmail) error {
	for i, data := range p.primaryEmails {
		if data.ID == primaryEmail.ID {
			p.primaryEmails[i] = primaryEmail
		}
	}
	return nil
}

func (p *primaryEmailPersister) Delete(primaryEmail models.PrimaryEmail) error {
	index := -1
	for i, data := range p.primaryEmails {
		if data.ID == primaryEmail.ID {
			index = i
		}
	}
	if index > -1 {
		p.primaryEmails = append(p.primaryEmails[:index], p.primaryEmails[index+1:]...)
	}

	return nil
}
