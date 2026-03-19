package contact

import (
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-vcard"
)

func ParseVCARD(r io.Reader) ([]Contact, error) {
	dec := vcard.NewDecoder(r)
	var contacts []Contact

	for {
		card, err := dec.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to decode vcard: %w", err)
		}

		c := Contact{
			Name:        card.PreferredValue(vcard.FieldFormattedName),
			Phone:       card.PreferredValue(vcard.FieldTelephone),
			Enabled:     strings.ToLower(card.Value("X-ENABLED")) == "true",
			Description: card.Value("X-DESCRIPTION"),
			Style:       card.Value("X-STYLE"),
		}

		if c.ID == "" {
			c.ID = c.Phone
		}

		contacts = append(contacts, c)
	}

	return contacts, nil
}
