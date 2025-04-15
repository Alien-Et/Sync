<template>
  <v-container>
    <v-row>
      <v-col>
        <v-card>
          <v-card-title>Resolve Conflicts</v-card-title>
          <v-card-text>
            <v-alert type="info">
              Conflicts are reported in the logs. Use the conflict ID from the log to resolve.
            </v-alert>
            <v-form @submit.prevent="resolve">
              <v-text-field v-model="conflictId" label="Conflict ID" required></v-text-field>
              <v-select v-model="resolution" :items="resolutions" label="Resolution" required></v-select>
              <v-btn color="primary" type="submit">Resolve</v-btn>
              <v-alert v-if="error" type="error">{{ error }}</v-alert>
              <v-alert v-if="success" type="success">Conflict resolved</v-alert>
            </v-form>
          </v-card-text>
        </v-card>
      </v-col>
    </v-row>
  </v-container>
</template>

<script>
export default {
  data() {
    return {
      conflictId: '',
      resolution: '',
      resolutions: ['local', 'remote', 'ignore'],
      error: '',
      success: false,
    }
  },
  methods: {
    async resolve() {
      try {
        const res = await fetch('/api/resolve-conflict', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            Authorization: `Bearer ${localStorage.getItem('token')}`,
          },
          body: JSON.stringify({ id: this.conflictId, resolution: this.resolution }),
        })
        if (res.ok) {
          this.success = true
          this.error = ''
          this.conflictId = ''
          this.resolution = ''
        } else {
          this.error = (await res.json()).error
          this.success = false
        }
      } catch (err) {
        this.error = 'Failed to resolve conflict'
        this.success = false
      }
    },
  },
}