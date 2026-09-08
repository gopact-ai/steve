package turn

import ()

func (c *Coordinator) lockSkills() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cancels) > 0 || c.skillsLock > 0 {
		return false
	}
	c.skillsLock++
	return true
}

func (c *Coordinator) unlockSkills() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.skillsLock > 0 {
		c.skillsLock--
	}
}

func (c *Coordinator) skillsUpdating() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.skillsLock > 0
}

// SkillsLock is the page's way in: skills may not change while a turn
// runs, since the change restarts the AI tools. The release is what the
// caller does when done; ok false means a turn is in flight.
func (c *Coordinator) SkillsLock() (release func(), ok bool) {
	if !c.lockSkills() {
		return nil, false
	}
	return c.unlockSkills, true
}
